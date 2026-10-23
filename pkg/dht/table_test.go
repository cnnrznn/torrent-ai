package dht

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/cnnrznn/torrent-ai/pkg/id"
)

// withPrefix returns an ID that shares exactly n leading bits with h.
func withPrefix(h id.Hash, n int) id.Hash {
	r := id.Random()
	for i := 0; i < n; i++ {
		mask := byte(0x80 >> (i % 8))
		r[i/8] = r[i/8]&^mask | h[i/8]&mask
	}
	mask := byte(0x80 >> (n % 8))
	r[n/8] = r[n/8]&^mask | (^h[n/8])&mask
	return r
}

func newNode(h id.Hash) node {
	return node{ID: h, Addr: netip.MustParseAddrPort("10.0.0.1:6881")}
}

func TestDistance(t *testing.T) {
	var a, b id.Hash
	b[19] = 0x05
	require.Equal(t, b, distance(a, b))
	require.Equal(t, 157, commonPrefixLen(a, b))
	require.Equal(t, 160, commonPrefixLen(a, a))

	self := id.Random()
	for _, n := range []int{0, 1, 7, 8, 100, 159} {
		require.Equal(t, n, commonPrefixLen(self, withPrefix(self, n)))
	}
}

func TestTable(t *testing.T) {
	t.Run("ignores our own ID", func(t *testing.T) {
		self := id.Random()
		tbl := newTable(self, K, defaultMaxFailures)
		require.False(t, tbl.insert(newNode(self)))
		require.Zero(t, tbl.len())
	})

	t.Run("splits the bucket containing our ID", func(t *testing.T) {
		self := id.Random()
		tbl := newTable(self, K, defaultMaxFailures)
		// Fill bucket 0 (far half), then add closer nodes.
		for range K {
			require.True(t, tbl.insert(newNode(withPrefix(self, 0))))
		}
		for range K {
			require.True(t, tbl.insert(newNode(withPrefix(self, 3))))
		}
		require.Equal(t, 2*K, tbl.len())
		require.Greater(t, len(tbl.buckets), 1)
	})

	t.Run("does not split far buckets", func(t *testing.T) {
		self := id.Random()
		tbl := newTable(self, K, defaultMaxFailures)
		for range K {
			tbl.insert(newNode(withPrefix(self, 0)))
		}
		tbl.insert(newNode(withPrefix(self, 5))) // forces a split
		require.False(t, tbl.insert(newNode(withPrefix(self, 0))), "bucket 0 is full")
	})

	t.Run("replaces bad nodes", func(t *testing.T) {
		self := id.Random()
		tbl := newTable(self, K, defaultMaxFailures)
		var far []id.Hash
		for range K {
			h := withPrefix(self, 0)
			far = append(far, h)
			tbl.insert(newNode(h))
		}
		tbl.insert(newNode(withPrefix(self, 5)))

		for range defaultMaxFailures {
			tbl.failed(far[0])
		}
		replacement := withPrefix(self, 0)
		require.True(t, tbl.insert(newNode(replacement)))
		require.NotContains(t, ids(tbl.closest(self, 100)), far[0])
		require.Contains(t, ids(tbl.closest(self, 100)), replacement)
	})

	t.Run("a reply clears failures", func(t *testing.T) {
		self := id.Random()
		tbl := newTable(self, K, defaultMaxFailures)
		h := id.Random()
		tbl.insert(newNode(h))
		tbl.failed(h)
		tbl.failed(h)
		require.Empty(t, tbl.closest(h, K))
		tbl.insert(newNode(h))
		require.Len(t, tbl.closest(h, K), 1)
	})

	t.Run("returns the closest nodes in order", func(t *testing.T) {
		self := id.Random()
		tbl := newTable(self, K, defaultMaxFailures)
		for range 200 {
			tbl.insert(newNode(id.Random()))
		}

		target := id.Random()
		got := tbl.closest(target, K)
		require.Len(t, got, K)
		for i := 1; i < len(got); i++ {
			a, b := distance(got[i-1].ID, target), distance(got[i].ID, target)
			require.Negative(t, compareHash(a, b))
		}
	})
}

func ids(nodes []node) []id.Hash {
	var out []id.Hash
	for _, n := range nodes {
		out = append(out, n.ID)
	}
	return out
}

func compareHash(a, b id.Hash) int {
	for i := range a {
		if a[i] != b[i] {
			return int(a[i]) - int(b[i])
		}
	}
	return 0
}

// defaultMaxFailures is the default bad-node threshold.
var defaultMaxFailures = DefaultConfig().MaxFailures

func TestTableBucketSize(t *testing.T) {
	self := id.Random()
	tbl := newTable(self, 2, defaultMaxFailures)
	for range 2 {
		tbl.insert(newNode(withPrefix(self, 0)))
	}
	tbl.insert(newNode(withPrefix(self, 5))) // splits
	require.False(t, tbl.insert(newNode(withPrefix(self, 0))), "bucket of 2 is full")
}
