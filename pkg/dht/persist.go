package dht

import (
	"net/netip"

	"github.com/cnnrznn/torrent-ai/pkg/id"
)

// NodeInfo is a node's ID and address, for saving the routing table
// between runs.
type NodeInfo struct {
	ID   string `json:"id"`   // hex
	Addr string `json:"addr"` // ip:port
}

// GoodNodes returns the routing table's nodes that are answering, to
// save across a restart.
func (s *Server) GoodNodes() []NodeInfo {
	var out []NodeInfo
	for _, n := range s.table.good() {
		out = append(out, NodeInfo{ID: n.ID.Hex(), Addr: n.Addr.String()})
	}
	return out
}

// Seed adds saved nodes to the routing table without contacting them,
// so a restarted node can look things up at once instead of
// bootstrapping from scratch. Nodes that have gone away are found out
// and replaced as lookups query them. It returns how many were added.
func (s *Server) Seed(nodes []NodeInfo) int {
	n := 0
	for _, ni := range nodes {
		h, err := id.FromHex(ni.ID)
		if err != nil {
			continue
		}
		addr, err := netip.ParseAddrPort(ni.Addr)
		if err != nil || !addr.Addr().Is4() {
			continue
		}
		if s.table.insert(node{ID: h, Addr: addr}) {
			n++
		}
	}
	return n
}
