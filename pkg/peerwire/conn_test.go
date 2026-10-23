package peerwire

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/cnnrznn/torrent-ai/pkg/id"
)

// connPair returns two connected Conns, one dialed and one accepted.
func connPair(t *testing.T, serverHash, clientHash id.Hash) (*Conn, *Conn, error) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	type result struct {
		c   *Conn
		err error
	}
	accepted := make(chan result, 1)
	go func() {
		nc, err := ln.Accept()
		if err != nil {
			accepted <- result{err: err}
			return
		}
		c, err := Accept(nc, NewHandshake(serverHash, id.NewPeerID(), false), Options{})
		accepted <- result{c, err}
	}()

	addr := ln.Addr().(*net.TCPAddr).AddrPort()
	client, err := Dial(context.Background(), addr, NewHandshake(clientHash, id.NewPeerID(), true), Options{})
	server := <-accepted
	if err != nil {
		return nil, nil, err
	}
	if server.err != nil {
		return nil, nil, server.err
	}
	return client, server.c, nil
}

func TestConn(t *testing.T) {
	t.Run("exchanges messages in both directions", func(t *testing.T) {
		infoHash := id.Random()
		client, server, err := connPair(t, infoHash, infoHash)
		require.NoError(t, err)
		defer client.Close()
		defer server.Close()

		require.True(t, server.Peer.SupportsDHT())
		require.False(t, client.Peer.SupportsDHT())

		require.NoError(t, client.Send(MsgHave(3)))
		m := <-server.Messages()
		require.Equal(t, Have, m.ID)

		require.NoError(t, server.Send(Message{ID: Unchoke}))
		m = <-client.Messages()
		require.Equal(t, Unchoke, m.ID)
	})

	t.Run("closing one side closes the other", func(t *testing.T) {
		infoHash := id.Random()
		client, server, err := connPair(t, infoHash, infoHash)
		require.NoError(t, err)

		client.Close()
		for range server.Messages() {
		}
		require.Error(t, server.Err())
		require.ErrorIs(t, client.Send(Message{ID: Choke}), ErrClosed)
	})

	t.Run("rejects a mismatched infohash", func(t *testing.T) {
		_, _, err := connPair(t, id.Random(), id.Random())
		require.Error(t, err)
	})
}

func TestConnOptions(t *testing.T) {
	t.Run("zero values take defaults", func(t *testing.T) {
		require.Equal(t, DefaultOptions(), Options{}.withDefaults())
		o := Options{SendQueue: 4}.withDefaults()
		require.Equal(t, 4, o.SendQueue)
		require.Equal(t, DefaultOptions().ReadTimeout, o.ReadTimeout)
	})

	t.Run("a silent peer is dropped after the read timeout", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer ln.Close()

		infoHash := id.Random()
		go func() {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			defer nc.Close()
			// Handshake, then go quiet.
			ReadHandshake(nc)
			nc.Write(NewHandshake(infoHash, id.NewPeerID(), false).Bytes())
			time.Sleep(5 * time.Second)
		}()

		addr := ln.Addr().(*net.TCPAddr).AddrPort()
		c, err := Dial(context.Background(), addr, NewHandshake(infoHash, id.NewPeerID(), false), Options{ReadTimeout: 100 * time.Millisecond})
		require.NoError(t, err)

		start := time.Now()
		for range c.Messages() {
		}
		require.Less(t, time.Since(start), 2*time.Second)
		require.ErrorContains(t, c.Err(), "timeout")
	})
}

func TestAcceptRouted(t *testing.T) {
	a, b := id.Random(), id.Random()
	handshakes := map[id.Hash]Handshake{
		a: NewHandshake(a, id.NewPeerID(), false),
		b: NewHandshake(b, id.NewPeerID(), true),
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	type result struct {
		c   *Conn
		err error
	}
	accepted := make(chan result, 3)
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				c, err := AcceptRouted(nc, func(h Handshake) (Handshake, bool) {
					hs, ok := handshakes[h.InfoHash]
					return hs, ok
				}, Options{})
				accepted <- result{c, err}
			}()
		}
	}()
	addr := ln.Addr().(*net.TCPAddr).AddrPort()

	for _, h := range []id.Hash{a, b} {
		client, err := Dial(context.Background(), addr, NewHandshake(h, id.NewPeerID(), false), Options{})
		require.NoError(t, err)
		require.Equal(t, handshakes[h].PeerID, client.Peer.PeerID, "answered with that torrent's handshake")
		r := <-accepted
		require.NoError(t, r.err)
		require.Equal(t, h, r.c.Peer.InfoHash)
		client.Close()
		r.c.Close()
	}

	_, err = Dial(context.Background(), addr, NewHandshake(id.Random(), id.NewPeerID(), false), Options{})
	require.Error(t, err)
	require.ErrorIs(t, (<-accepted).err, ErrUnknownInfoHash)
}
