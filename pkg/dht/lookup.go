package dht

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cnnrznn/torrent-ai/pkg/id"
)

// alpha is the number of queries a lookup keeps in flight.
const alpha = 3

// bootstrapAlpha is the parallelism for the bootstrap lookup. Many
// nodes a fresh client learns about are unreachable, and each one
// holds a slot until its query times out, so a wider lookup joins the
// DHT much faster.
const bootstrapAlpha = 16

// DefaultRouters are well-known nodes to bootstrap from.
var DefaultRouters = []string{
	"router.bittorrent.com:6881",
	"dht.transmissionbt.com:6881",
}

var errNoNodes = errors.New("routing table is empty")

type candidate struct {
	node
	queried   bool
	responded bool
	failed    bool
	token     string
}

// LookupStats summarizes one lookup.
type LookupStats struct {
	Queried   int // nodes queried
	Responded int // nodes that answered
	Failed    int // nodes that timed out or returned an error
	Peers     int // distinct peers found
	Announced int // nodes that accepted our announce_peer
	// ClosestBits is how many leading bits the closest responding node
	// shares with the target. On the live DHT a converged lookup gets
	// around 20 or more; a small value means the lookup stalled far
	// from the target.
	ClosestBits int
	Duration    time.Duration
}

// LogValue lets a LookupStats be logged as a group.
func (st LookupStats) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int("queried", st.Queried),
		slog.Int("responded", st.Responded),
		slog.Int("failed", st.Failed),
		slog.Int("peers", st.Peers),
		slog.Int("announced", st.Announced),
		slog.Int("closest_bits", st.ClosestBits),
		slog.Duration("dur", st.Duration),
	)
}

type lookupResult struct {
	c     *candidate
	nodes []node
	peers []netip.AddrPort
	token string
	err   error
}

// lookup walks toward target, querying the closest known nodes
// parallel at a time, until the K closest nodes that answered have all been
// queried. With getPeers it sends get_peers and streams any peers found
// to found; otherwise it sends find_node. It returns the closest nodes
// that answered, nearest first.
func (s *Server) lookup(ctx context.Context, target id.Hash, getPeers bool, found func([]netip.AddrPort), seeds []node, parallel int) ([]*candidate, LookupStats, error) {
	start := time.Now()
	var st LookupStats
	seen := map[netip.AddrPort]bool{}

	cands := map[id.Hash]*candidate{}
	add := func(n node) {
		if n.ID == s.id {
			return
		}
		if _, ok := cands[n.ID]; !ok {
			cands[n.ID] = &candidate{node: n}
		}
	}
	for _, n := range s.table.closest(target, K) {
		add(n)
	}
	for _, n := range seeds {
		add(n)
	}
	if len(cands) == 0 {
		return nil, st, errNoNodes
	}

	closest := func() []*candidate {
		var list []*candidate
		var nodes []node
		for _, c := range cands {
			if !c.failed {
				nodes = append(nodes, c.node)
			}
		}
		sortByDistance(nodes, target)
		for _, n := range nodes[:min(K, len(nodes))] {
			list = append(list, cands[n.ID])
		}
		return list
	}

	results := make(chan lookupResult, parallel)
	inflight := 0

	for {
		for _, c := range closest() {
			if inflight >= parallel {
				break
			}
			if c.queried {
				continue
			}
			c.queried = true
			inflight++
			st.Queried++
			go func() {
				r := lookupResult{c: c}
				if getPeers {
					var res getPeersResult
					res, r.err = s.getPeers(ctx, c.node, target)
					r.nodes, r.peers, r.token = res.nodes, res.peers, res.token
				} else {
					r.nodes, r.err = s.findNode(ctx, c.Addr, target)
					if r.err != nil && ctx.Err() == nil {
						s.table.failed(c.ID)
					}
				}
				results <- r
			}()
		}

		if inflight == 0 {
			break
		}

		var r lookupResult
		select {
		case r = <-results:
		case <-ctx.Done():
			st.Duration = time.Since(start)
			return nil, st, ctx.Err()
		}
		inflight--

		if r.err != nil {
			r.c.failed = true
			st.Failed++
			continue
		}
		r.c.responded = true
		st.Responded++
		r.c.token = r.token
		for _, n := range r.nodes {
			add(n)
		}
		for _, p := range r.peers {
			if !seen[p] {
				seen[p] = true
				st.Peers++
			}
		}
		if len(r.peers) > 0 && found != nil {
			found(r.peers)
		}
	}

	result := closest()
	for _, c := range result {
		if c.responded {
			st.ClosestBits = commonPrefixLen(c.ID, target)
			break
		}
	}
	st.Duration = time.Since(start)
	return result, st, nil
}

// Bootstrap joins the DHT through the given router addresses
// ("host:port"), then looks up our own ID to fill the routing table.
func (s *Server) Bootstrap(ctx context.Context, routers []string) (LookupStats, error) {
	var mu sync.Mutex
	var seeds []node
	var wg sync.WaitGroup

	for _, r := range routers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			addr, err := net.ResolveUDPAddr("udp4", r)
			if err != nil {
				s.log.Warn("dht router", "router", r, "result", "resolve failed", "err", err)
				return
			}
			ap := netip.AddrPortFrom(addr.AddrPort().Addr().Unmap(), addr.AddrPort().Port())
			nodes, err := s.findNode(ctx, ap, s.id)
			if err != nil {
				s.log.Warn("dht router", "router", r, "addr", ap, "result", "failed", "err", err, "dur", time.Since(start))
				return
			}
			s.log.Debug("dht router", "router", r, "addr", ap, "result", "ok", "nodes", len(nodes), "dur", time.Since(start))
			mu.Lock()
			seeds = append(seeds, nodes...)
			mu.Unlock()
		}()
	}
	wg.Wait()

	_, st, err := s.lookup(ctx, s.id, false, nil, seeds, bootstrapAlpha)
	if err != nil {
		return st, err
	}
	if s.table.len() == 0 {
		return st, errNoNodes
	}
	return st, nil
}

// GetPeers looks up peers for infoHash, calling found with each batch
// as it arrives.
func (s *Server) GetPeers(ctx context.Context, infoHash id.Hash, found func([]netip.AddrPort)) (LookupStats, error) {
	_, st, err := s.lookup(ctx, infoHash, true, found, nil, alpha)
	return st, err
}

// Announce looks up peers for infoHash like GetPeers, then announces
// that we accept peer connections on port to the closest nodes that
// gave us a token.
func (s *Server) Announce(ctx context.Context, infoHash id.Hash, port int, found func([]netip.AddrPort)) (LookupStats, error) {
	start := time.Now()
	closest, st, err := s.lookup(ctx, infoHash, true, found, nil, alpha)
	if err != nil {
		return st, err
	}

	var announced atomic.Int64
	var wg sync.WaitGroup
	for _, c := range closest {
		if c.token == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.announcePeer(ctx, c.node, infoHash, port, c.token); err != nil {
				return
			}
			announced.Add(1)
		}()
	}
	wg.Wait()

	st.Announced = int(announced.Load())
	st.Duration = time.Since(start)
	return st, nil
}

// AddNode pings addr in the background and adds it to the routing
// table if it answers. Use it for nodes learned outside the DHT, such
// as a peer's wire protocol port message.
func (s *Server) AddNode(addr netip.AddrPort) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*s.timeout+time.Second)
		defer cancel()
		nodeID, err := s.Ping(ctx, addr)
		if err != nil {
			s.log.Debug("dht add node", "addr", addr, "result", "failed", "err", err)
			return
		}
		s.log.Debug("dht add node", "addr", addr, "result", "ok", "id", nodeID.Hex(), "nodes", s.table.len())
	}()
}
