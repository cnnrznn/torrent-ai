package dht

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/cnnrznn/torrent-scratch/pkg/id"
)

// startNetwork starts n nodes that bootstrap from the first.
func startNetwork(t *testing.T, ctx context.Context, n int) []*Server {
	t.Helper()

	nodes := []*Server{startServer(t, ctx)}
	router := []string{nodes[0].Addr().String()}
	for range n - 1 {
		s := startServer(t, ctx)
		require.NoError(t, s.Bootstrap(ctx, router))
		nodes = append(nodes, s)
	}
	return nodes
}

func TestLookup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	nodes := startNetwork(t, ctx, 30)

	t.Run("bootstrap fills routing tables", func(t *testing.T) {
		for _, s := range nodes[1:] {
			require.Greater(t, s.Nodes(), 1)
		}
	})

	t.Run("find_node lookup converges on the closest nodes", func(t *testing.T) {
		target := id.Random()
		got, err := nodes[7].lookup(ctx, target, false, nil, nil, alpha)
		require.NoError(t, err)

		var all []node
		for _, s := range nodes {
			if s != nodes[7] {
				all = append(all, asNode(s))
			}
		}
		sortByDistance(all, target)
		require.Equal(t, ids(all[:K]), ids(candidateNodes(got)))
	})

	t.Run("a peer announced on one node is found from another", func(t *testing.T) {
		infoHash := id.Random()
		require.NoError(t, nodes[5].Announce(ctx, infoHash, 7000, nil))

		var mu sync.Mutex
		found := map[netip.AddrPort]bool{}
		err := nodes[20].GetPeers(ctx, infoHash, func(peers []netip.AddrPort) {
			mu.Lock()
			defer mu.Unlock()
			for _, p := range peers {
				found[p] = true
			}
		})
		require.NoError(t, err)
		require.True(t, found[netip.AddrPortFrom(nodes[5].Addr().Addr(), 7000)], "found %v", found)
	})

	t.Run("lookup with an empty table fails", func(t *testing.T) {
		lonely := startServer(t, ctx)
		require.ErrorIs(t, lonely.GetPeers(ctx, id.Random(), nil), errNoNodes)
	})

	t.Run("AddNode adds a node that answers", func(t *testing.T) {
		s := startServer(t, ctx)
		s.AddNode(nodes[3].Addr())
		require.Eventually(t, func() bool { return s.Nodes() == 1 }, 5*time.Second, 10*time.Millisecond)
	})
}

func candidateNodes(cs []*candidate) []node {
	var out []node
	for _, c := range cs {
		out = append(out, c.node)
	}
	return out
}
