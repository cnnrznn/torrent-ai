package dht

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/cnnrznn/torrent-ai/pkg/bencoding"
	"github.com/cnnrznn/torrent-ai/pkg/id"
)

func startServer(t *testing.T, ctx context.Context) *Server {
	t.Helper()
	s, err := New(Config{Addr: "127.0.0.1:0", QueryTimeout: 500 * time.Millisecond})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return s
}

func asNode(s *Server) node {
	return node{ID: s.ID(), Addr: s.Addr()}
}

func TestServer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a, b := startServer(t, ctx), startServer(t, ctx)

	t.Run("ping adds both nodes to each other's tables", func(t *testing.T) {
		got, err := a.Ping(ctx, b.Addr())
		require.NoError(t, err)
		require.Equal(t, b.ID(), got)
		require.Contains(t, ids(a.table.closest(b.ID(), K)), b.ID())
		require.Contains(t, ids(b.table.closest(a.ID(), K)), a.ID())
	})

	t.Run("find_node returns known nodes", func(t *testing.T) {
		known := node{ID: id.Random(), Addr: netip.MustParseAddrPort("10.1.2.3:4567")}
		b.table.insert(known)

		nodes, err := a.findNode(ctx, b.Addr(), known.ID)
		require.NoError(t, err)
		require.Contains(t, nodes, known)
	})

	t.Run("announce_peer then get_peers returns the peer", func(t *testing.T) {
		infoHash := id.Random()

		res, err := a.getPeers(ctx, asNode(b), infoHash)
		require.NoError(t, err)
		require.Empty(t, res.peers)
		require.NotEmpty(t, res.token)

		require.NoError(t, a.announcePeer(ctx, asNode(b), infoHash, 51413, res.token))

		res, err = a.getPeers(ctx, asNode(b), infoHash)
		require.NoError(t, err)
		require.Equal(t, []netip.AddrPort{netip.AddrPortFrom(a.Addr().Addr(), 51413)}, res.peers)
	})

	t.Run("announce_peer rejects a bad token", func(t *testing.T) {
		err := a.announcePeer(ctx, asNode(b), id.Random(), 1, "forged")
		require.ErrorContains(t, err, "bad token")
	})

	t.Run("unknown methods get an error", func(t *testing.T) {
		_, _, err := a.query(ctx, b.Addr(), "vote", map[string]string{})
		require.ErrorContains(t, err, "204")
	})

	t.Run("queries missing arguments get an error", func(t *testing.T) {
		_, _, err := a.query(ctx, b.Addr(), "get_peers", map[string]string{"info_hash": bencoding.EncodeString("short")})
		require.ErrorContains(t, err, "203")
	})

	t.Run("an unanswered query times out and marks the node", func(t *testing.T) {
		gone := startServer(t, ctx)
		n := asNode(gone)
		a.table.insert(n)

		// Close the socket so it can't answer.
		gone.conn.Close()
		for range maxFailures {
			_, err := a.getPeers(ctx, n, id.Random())
			require.ErrorContains(t, err, "timed out")
		}
		require.NotContains(t, ids(a.table.closest(n.ID, 100)), n.ID)
		require.GreaterOrEqual(t, a.Stats().Timeouts, int64(maxFailures))
	})
}

func TestTokens(t *testing.T) {
	s, err := New(Config{Addr: "127.0.0.1:0"})
	require.NoError(t, err)
	defer s.conn.Close()

	ip := netip.MustParseAddr("1.2.3.4")
	tok := s.token(ip, s.currentSecret())
	require.True(t, s.validToken(ip, tok))
	require.False(t, s.validToken(netip.MustParseAddr("1.2.3.5"), tok))

	// Still valid after one rotation, not after two.
	s.prevSecret, s.secret = s.secret, [16]byte{1}
	require.True(t, s.validToken(ip, tok))
	s.prevSecret, s.secret = s.secret, [16]byte{2}
	require.False(t, s.validToken(ip, tok))
}
