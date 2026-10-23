package tracker

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"
)

// UDP tracker protocol, BEP 15.
const (
	udpProtocolID = 0x41727101980

	actionConnect  = 0
	actionAnnounce = 1
	actionError    = 3
)

type udpTracker struct {
	host    string
	timeout time.Duration
	retries int
	connTTL time.Duration
	log     *slog.Logger
}

var errTimeout = errors.New("udp tracker timed out")

// Announce connects to the tracker and announces, retrying with the
// BEP 15 backoff when a reply doesn't arrive in time.
func (t *udpTracker) Announce(ctx context.Context, r Request) (*Response, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", t.host)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// Unblock reads when the context is cancelled.
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) })
	defer stop()

	var connID uint64
	var connAt time.Time

	for n := 0; n <= t.retries; n++ {
		timeout := t.timeout << n

		if connAt.IsZero() || time.Since(connAt) > t.connTTL {
			t.log.Debug("udp tracker connect", "host", t.host, "attempt", n, "timeout", timeout)
			connID, err = t.connect(conn, timeout)
			if errors.Is(err, errTimeout) && ctx.Err() == nil {
				t.log.Debug("udp tracker timeout", "host", t.host, "step", "connect", "attempt", n, "timeout", timeout)
				continue
			}
			if err != nil {
				return nil, errors.Join(ctx.Err(), err)
			}
			connAt = time.Now()
		}

		t.log.Debug("udp tracker announce", "host", t.host, "attempt", n, "timeout", timeout, "event", r.Event)
		res, err := t.announce(conn, connID, r, timeout)
		if errors.Is(err, errTimeout) && ctx.Err() == nil {
			t.log.Debug("udp tracker timeout", "host", t.host, "step", "announce", "attempt", n, "timeout", timeout)
			continue
		}
		if err != nil {
			return nil, errors.Join(ctx.Err(), err)
		}
		t.log.Debug("udp tracker response", "host", t.host, "peers", len(res.Peers), "interval", res.Interval)
		return res, nil
	}

	return nil, errTimeout
}

func (t *udpTracker) connect(conn net.Conn, timeout time.Duration) (uint64, error) {
	tx := newTransactionID()
	req := binary.BigEndian.AppendUint64(nil, udpProtocolID)
	req = binary.BigEndian.AppendUint32(req, actionConnect)
	req = binary.BigEndian.AppendUint32(req, tx)

	res, err := roundTrip(conn, req, actionConnect, tx, timeout)
	if err != nil {
		return 0, err
	}
	if len(res) < 16 {
		return 0, fmt.Errorf("connect response is %d bytes", len(res))
	}
	return binary.BigEndian.Uint64(res[8:16]), nil
}

func (t *udpTracker) announce(conn net.Conn, connID uint64, r Request, timeout time.Duration) (*Response, error) {
	tx := newTransactionID()
	req := binary.BigEndian.AppendUint64(nil, connID)
	req = binary.BigEndian.AppendUint32(req, actionAnnounce)
	req = binary.BigEndian.AppendUint32(req, tx)
	req = append(req, r.InfoHash[:]...)
	req = append(req, r.PeerID[:]...)
	req = binary.BigEndian.AppendUint64(req, uint64(r.Downloaded))
	req = binary.BigEndian.AppendUint64(req, uint64(r.Left))
	req = binary.BigEndian.AppendUint64(req, uint64(r.Uploaded))
	req = binary.BigEndian.AppendUint32(req, udpEvent(r.Event))
	req = binary.BigEndian.AppendUint32(req, 0)                  // IP: use the sender's
	req = binary.BigEndian.AppendUint32(req, newTransactionID()) // key
	numWant := int32(-1)                                         // the tracker's default
	if r.NumWant > 0 {
		numWant = int32(r.NumWant)
	}
	req = binary.BigEndian.AppendUint32(req, uint32(numWant))
	req = binary.BigEndian.AppendUint16(req, uint16(r.Port))

	res, err := roundTrip(conn, req, actionAnnounce, tx, timeout)
	if err != nil {
		return nil, err
	}
	if len(res) < 20 {
		return nil, fmt.Errorf("announce response is %d bytes", len(res))
	}

	interval := binary.BigEndian.Uint32(res[8:12])
	peers, err := ParseCompactPeers(string(res[20:]))
	if err != nil {
		return nil, err
	}

	return &Response{
		Interval: time.Duration(interval) * time.Second,
		Peers:    peers,
	}, nil
}

// roundTrip sends req and waits up to timeout for a reply with the
// same transaction ID. Replies for other transactions are ignored.
func roundTrip(conn net.Conn, req []byte, action, tx uint32, timeout time.Duration) ([]byte, error) {
	if _, err := conn.Write(req); err != nil {
		return nil, err
	}

	deadline := time.Now().Add(timeout)
	buf := make([]byte, 2048)
	for {
		conn.SetReadDeadline(deadline)
		n, err := conn.Read(buf)
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return nil, errTimeout
		}
		if err != nil {
			return nil, err
		}

		res := buf[:n]
		if len(res) < 8 || binary.BigEndian.Uint32(res[4:8]) != tx {
			continue
		}

		switch got := binary.BigEndian.Uint32(res[:4]); got {
		case action:
			return res, nil
		case actionError:
			return nil, fmt.Errorf("udp tracker error: %s", res[8:])
		default:
			return nil, fmt.Errorf("udp tracker replied with action %d, want %d", got, action)
		}
	}
}

func udpEvent(e Event) uint32 {
	switch e {
	case EventCompleted:
		return 1
	case EventStarted:
		return 2
	case EventStopped:
		return 3
	}
	return 0
}

func newTransactionID() uint32 {
	var b [4]byte
	rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])
}
