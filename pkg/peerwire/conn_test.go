package peerwire

import (
	"context"
	"net"
	"testing"

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
		c, err := Accept(nc, NewHandshake(serverHash, id.NewPeerID(), false))
		accepted <- result{c, err}
	}()

	addr := ln.Addr().(*net.TCPAddr).AddrPort()
	client, err := Dial(context.Background(), addr, NewHandshake(clientHash, id.NewPeerID(), true))
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
