package dht

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/cnnrznn/torrent-scratch/pkg/id"
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
	queried bool
	failed  bool
	token   string
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
func (s *Server) lookup(ctx context.Context, target id.Hash, getPeers bool, found func([]netip.AddrPort), seeds []node, parallel int) ([]*candidate, error) {
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
		return nil, errNoNodes
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
			return nil, ctx.Err()
		}
		inflight--

		if r.err != nil {
			r.c.failed = true
			continue
		}
		r.c.token = r.token
		for _, n := range r.nodes {
			add(n)
		}
		if len(r.peers) > 0 && found != nil {
			found(r.peers)
		}
	}

	return closest(), nil
}

// Bootstrap joins the DHT through the given router addresses
// ("host:port"), then looks up our own ID to fill the routing table.
func (s *Server) Bootstrap(ctx context.Context, routers []string) error {
	var mu sync.Mutex
	var seeds []node
	var wg sync.WaitGroup

	for _, r := range routers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			addr, err := net.ResolveUDPAddr("udp4", r)
			if err != nil {
				s.log.Debug("resolve router failed", "router", r, "err", err)
				return
			}
			ap := netip.AddrPortFrom(addr.AddrPort().Addr().Unmap(), addr.AddrPort().Port())
			nodes, err := s.findNode(ctx, ap, s.id)
			if err != nil {
				s.log.Debug("router failed", "router", r, "err", err)
				return
			}
			mu.Lock()
			seeds = append(seeds, nodes...)
			mu.Unlock()
		}()
	}
	wg.Wait()

	if _, err := s.lookup(ctx, s.id, false, nil, seeds, bootstrapAlpha); err != nil {
		return err
	}
	if s.table.len() == 0 {
		return errNoNodes
	}
	s.log.Debug("bootstrapped", "nodes", s.table.len())
	return nil
}

// GetPeers looks up peers for infoHash, calling found with each batch
// as it arrives.
func (s *Server) GetPeers(ctx context.Context, infoHash id.Hash, found func([]netip.AddrPort)) error {
	_, err := s.lookup(ctx, infoHash, true, found, nil, alpha)
	return err
}

// Announce looks up peers for infoHash like GetPeers, then announces
// that we accept peer connections on port to the closest nodes that
// gave us a token.
func (s *Server) Announce(ctx context.Context, infoHash id.Hash, port int, found func([]netip.AddrPort)) error {
	closest, err := s.lookup(ctx, infoHash, true, found, nil, alpha)
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	for _, c := range closest {
		if c.token == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.announcePeer(ctx, c.node, infoHash, port, c.token); err != nil {
				s.log.Debug("announce_peer failed", "addr", c.Addr, "err", err)
			}
		}()
	}
	wg.Wait()
	return nil
}

// AddNode pings addr in the background and adds it to the routing
// table if it answers. Use it for nodes learned outside the DHT, such
// as a peer's wire protocol port message.
func (s *Server) AddNode(addr netip.AddrPort) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*s.timeout+time.Second)
		defer cancel()
		s.Ping(ctx, addr)
	}()
}
