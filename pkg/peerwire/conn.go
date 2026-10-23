package peerwire

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"
)

// Options tunes a connection. Zero fields take the defaults in
// DefaultOptions.
type Options struct {
	// HandshakeTimeout bounds connecting and exchanging handshakes.
	HandshakeTimeout time.Duration
	// KeepAlive is how long the connection may go without us writing
	// before we send a keep-alive.
	KeepAlive time.Duration
	// ReadTimeout closes the connection if the peer sends nothing,
	// not even a keep-alive, for this long.
	ReadTimeout time.Duration
	// WriteTimeout bounds each write.
	WriteTimeout time.Duration
	// SendQueue is how many outgoing messages may be queued before the
	// peer is considered too slow and disconnected.
	SendQueue int
	// MaxMessageSize is the largest message accepted from the peer.
	MaxMessageSize int
}

// DefaultOptions returns the default connection settings.
func DefaultOptions() Options {
	return Options{
		HandshakeTimeout: 10 * time.Second,
		KeepAlive:        2 * time.Minute,
		ReadTimeout:      3 * time.Minute,
		WriteTimeout:     30 * time.Second,
		SendQueue:        256,
		MaxMessageSize:   MaxMessageLen,
	}
}

func (o Options) withDefaults() Options {
	d := DefaultOptions()
	if o.HandshakeTimeout <= 0 {
		o.HandshakeTimeout = d.HandshakeTimeout
	}
	if o.KeepAlive <= 0 {
		o.KeepAlive = d.KeepAlive
	}
	if o.ReadTimeout <= 0 {
		o.ReadTimeout = d.ReadTimeout
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = d.WriteTimeout
	}
	if o.SendQueue <= 0 {
		o.SendQueue = d.SendQueue
	}
	if o.MaxMessageSize <= 0 {
		o.MaxMessageSize = d.MaxMessageSize
	}
	return o
}

// ErrClosed is returned by Send after the connection has closed.
var ErrClosed = errors.New("connection closed")

// Conn is an established peer connection. Incoming messages are
// delivered on Messages; outgoing messages are queued with Send and
// written by a separate goroutine, so neither direction blocks the
// other.
type Conn struct {
	// Peer is the handshake the remote peer sent.
	Peer Handshake

	nc   net.Conn
	opts Options
	msgs chan Message
	out  chan []byte
	done chan struct{}

	closeOnce sync.Once
	err       error
}

// Dial connects to addr and exchanges handshakes. It fails if the
// peer answers for a different infohash.
func Dial(ctx context.Context, addr netip.AddrPort, local Handshake, opts Options) (*Conn, error) {
	opts = opts.withDefaults()
	d := net.Dialer{Timeout: opts.HandshakeTimeout}
	nc, err := d.DialContext(ctx, "tcp", addr.String())
	if err != nil {
		return nil, err
	}

	nc.SetDeadline(time.Now().Add(opts.HandshakeTimeout))
	if _, err := nc.Write(local.Bytes()); err != nil {
		nc.Close()
		return nil, err
	}
	remote, err := ReadHandshake(nc)
	if err != nil {
		nc.Close()
		return nil, err
	}
	if remote.InfoHash != local.InfoHash {
		nc.Close()
		return nil, errors.New("peer sent a different infohash")
	}
	nc.SetDeadline(time.Time{})

	return newConn(nc, remote, opts), nil
}

// Accept completes the handshake on an incoming connection. The remote
// peer speaks first; we answer only if it wants local.InfoHash.
func Accept(nc net.Conn, local Handshake, opts Options) (*Conn, error) {
	opts = opts.withDefaults()
	nc.SetDeadline(time.Now().Add(opts.HandshakeTimeout))
	remote, err := ReadHandshake(nc)
	if err != nil {
		nc.Close()
		return nil, err
	}
	if remote.InfoHash != local.InfoHash {
		nc.Close()
		return nil, errors.New("peer asked for an unknown infohash")
	}
	if _, err := nc.Write(local.Bytes()); err != nil {
		nc.Close()
		return nil, err
	}
	nc.SetDeadline(time.Time{})

	return newConn(nc, remote, opts), nil
}

func newConn(nc net.Conn, remote Handshake, opts Options) *Conn {
	c := &Conn{
		Peer: remote,
		nc:   nc,
		opts: opts,
		msgs: make(chan Message),
		out:  make(chan []byte, opts.SendQueue),
		done: make(chan struct{}),
	}
	go c.readLoop()
	go c.writeLoop()
	return c
}

// RemoteAddr returns the peer's address.
func (c *Conn) RemoteAddr() netip.AddrPort {
	if a, ok := c.nc.RemoteAddr().(*net.TCPAddr); ok {
		return a.AddrPort()
	}
	return netip.AddrPort{}
}

// Messages returns the channel of incoming messages. It is closed when
// the connection closes; Err then reports why.
func (c *Conn) Messages() <-chan Message {
	return c.msgs
}

// Send queues m for writing. It never blocks: if the peer isn't
// reading fast enough to drain the queue, the connection is closed.
func (c *Conn) Send(m Message) error {
	return c.send(m.Bytes())
}

func (c *Conn) send(b []byte) error {
	select {
	case <-c.done:
		return ErrClosed
	default:
	}

	select {
	case c.out <- b:
		return nil
	default:
		c.closeWithError(errors.New("send queue full"))
		return ErrClosed
	}
}

// Done is closed when the connection closes.
func (c *Conn) Done() <-chan struct{} {
	return c.done
}

// Err returns the reason the connection closed. It is only valid
// after Done is closed.
func (c *Conn) Err() error {
	<-c.done
	return c.err
}

// Close closes the connection.
func (c *Conn) Close() error {
	c.closeWithError(ErrClosed)
	return nil
}

func (c *Conn) closeWithError(err error) {
	c.closeOnce.Do(func() {
		c.err = err
		close(c.done)
		c.nc.Close()
	})
}

func (c *Conn) readLoop() {
	defer close(c.msgs)

	r := bufio.NewReaderSize(c.nc, 32*1024)
	for {
		c.nc.SetReadDeadline(time.Now().Add(c.opts.ReadTimeout))
		m, err := ReadMessage(r, c.opts.MaxMessageSize)
		if err != nil {
			c.closeWithError(fmt.Errorf("read: %w", err))
			return
		}
		if m == nil {
			continue
		}

		select {
		case c.msgs <- *m:
		case <-c.done:
			return
		}
	}
}

func (c *Conn) writeLoop() {
	keepAlive := time.NewTicker(c.opts.KeepAlive)
	defer keepAlive.Stop()

	for {
		var b []byte
		select {
		case b = <-c.out:
		case <-keepAlive.C:
			b = KeepAlive
		case <-c.done:
			return
		}

		c.nc.SetWriteDeadline(time.Now().Add(c.opts.WriteTimeout))
		if _, err := c.nc.Write(b); err != nil {
			c.closeWithError(fmt.Errorf("write: %w", err))
			return
		}
		keepAlive.Reset(c.opts.KeepAlive)
	}
}
