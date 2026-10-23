package dht

import (
	"bytes"
	"math/bits"
	"slices"
	"sync"
	"time"

	"github.com/cnnrznn/torrent-ai/pkg/id"
)

const (
	// K is the bucket size and the number of nodes a lookup converges on.
	K = 8
	// maxFailures is how many unanswered queries make a node bad. Bad
	// nodes are left out of lookups and replaced first.
	maxFailures = 2
)

type entry struct {
	node
	lastSeen time.Time
	failures int
}

// table is a Kademlia routing table. Bucket i holds nodes whose ID
// shares exactly i leading bits with ours, except the last bucket,
// which holds everything closer. Only that last bucket (the one
// containing our own ID) is split when it fills up.
type table struct {
	mu      sync.Mutex
	self    id.Hash
	buckets [][]*entry
}

func newTable(self id.Hash) *table {
	return &table{self: self, buckets: make([][]*entry, 1)}
}

// distance is the XOR metric.
func distance(a, b id.Hash) id.Hash {
	var d id.Hash
	for i := range d {
		d[i] = a[i] ^ b[i]
	}
	return d
}

// commonPrefixLen returns the number of leading bits a and b share.
func commonPrefixLen(a, b id.Hash) int {
	for i := range a {
		if x := a[i] ^ b[i]; x != 0 {
			return i*8 + bits.LeadingZeros8(x)
		}
	}
	return 8 * len(a)
}

func (t *table) bucketIndex(h id.Hash) int {
	return min(commonPrefixLen(t.self, h), len(t.buckets)-1)
}

// insert records that n is alive. It reports whether n is in the table
// afterwards.
func (t *table) insert(n node) bool {
	if n.ID == t.self {
		return false
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	for {
		i := t.bucketIndex(n.ID)
		b := t.buckets[i]

		for _, e := range b {
			if e.ID == n.ID {
				e.lastSeen = now
				e.failures = 0
				return true
			}
		}

		if len(b) < K {
			t.buckets[i] = append(b, &entry{node: n, lastSeen: now})
			return true
		}

		if i == len(t.buckets)-1 && len(t.buckets) < 8*len(t.self) {
			t.split()
			continue
		}

		for j, e := range b {
			if e.failures >= maxFailures {
				b[j] = &entry{node: n, lastSeen: now}
				return true
			}
		}
		return false
	}
}

// split divides the last bucket in two: nodes sharing one more bit
// with our ID move to a new last bucket.
func (t *table) split() {
	last := len(t.buckets) - 1
	var keep, move []*entry
	for _, e := range t.buckets[last] {
		if commonPrefixLen(t.self, e.ID) > last {
			move = append(move, e)
		} else {
			keep = append(keep, e)
		}
	}
	t.buckets[last] = keep
	t.buckets = append(t.buckets, move)
}

// failed records that a query to the node with this ID went
// unanswered.
func (t *table) failed(h id.Hash) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, e := range t.buckets[t.bucketIndex(h)] {
		if e.ID == h {
			e.failures++
			return
		}
	}
}

// closest returns up to k good nodes nearest to target, nearest first.
func (t *table) closest(target id.Hash, k int) []node {
	t.mu.Lock()
	var nodes []node
	for _, b := range t.buckets {
		for _, e := range b {
			if e.failures < maxFailures {
				nodes = append(nodes, e.node)
			}
		}
	}
	t.mu.Unlock()

	sortByDistance(nodes, target)
	return nodes[:min(k, len(nodes))]
}

func sortByDistance(nodes []node, target id.Hash) {
	slices.SortFunc(nodes, func(a, b node) int {
		da, db := distance(a.ID, target), distance(b.ID, target)
		return bytes.Compare(da[:], db[:])
	})
}

// len returns the number of nodes in the table.
func (t *table) len() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	n := 0
	for _, b := range t.buckets {
		n += len(b)
	}
	return n
}
