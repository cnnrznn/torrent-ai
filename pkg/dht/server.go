package dht

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cnnrznn/torrent-scratch/pkg/bencoding"
	"github.com/cnnrznn/torrent-scratch/pkg/id"
)

const (
	defaultQueryTimeout = 3 * time.Second
	tokenRotation       = 5 * time.Minute
	peerTTL             = 30 * time.Minute
	maxValues           = 50
	maxPeersPerHash     = 1000
)

// Config configures a Server.
type Config struct {
	// Addr is the UDP address to listen on, e.g. ":6881".
	Addr string
	// ID is our node ID. A random one is used if zero.
	ID id.Hash
	// QueryTimeout bounds each outgoing query. Defaults to 3s.
	QueryTimeout time.Duration
	// Logger receives debug logs. Defaults to discarding.
	Logger *slog.Logger
}

// Server is a DHT node. It answers queries from other nodes and sends
// its own.
type Server struct {
	id      id.Hash
	conn    *net.UDPConn
	table   *table
	timeout time.Duration
	log     *slog.Logger

	mu      sync.Mutex
	pending map[string]*pendingQuery
	nextTx  uint16

	tokenMu    sync.Mutex
	secret     [16]byte
	prevSecret [16]byte

	peersMu sync.Mutex
	peers   map[id.Hash]map[netip.AddrPort]time.Time

	stats counters
}

type counters struct {
	sent, replies, errorReplies, timeouts atomic.Int64
	received, badPackets, announced       atomic.Int64
}

// Stats is a snapshot of a server's activity since it started.
type Stats struct {
	// Nodes and Buckets describe the routing table.
	Nodes   int
	Buckets int
	// StoredPeers is how many peers other nodes have announced to us.
	StoredPeers int

	QueriesSent  int64 // queries we sent
	Replies      int64 // responses to our queries
	ErrorReplies int64 // KRPC errors in reply to our queries
	Timeouts     int64 // our queries that went unanswered

	QueriesReceived int64 // queries other nodes sent us
	Announced       int64 // announce_peer queries we accepted
	BadPackets      int64 // packets we couldn't decode
}

// Stats returns a snapshot of the server's activity.
func (s *Server) Stats() Stats {
	s.table.mu.Lock()
	buckets := len(s.table.buckets)
	s.table.mu.Unlock()

	s.peersMu.Lock()
	stored := 0
	for _, set := range s.peers {
		stored += len(set)
	}
	s.peersMu.Unlock()

	return Stats{
		Nodes:           s.table.len(),
		Buckets:         buckets,
		StoredPeers:     stored,
		QueriesSent:     s.stats.sent.Load(),
		Replies:         s.stats.replies.Load(),
		ErrorReplies:    s.stats.errorReplies.Load(),
		Timeouts:        s.stats.timeouts.Load(),
		QueriesReceived: s.stats.received.Load(),
		Announced:       s.stats.announced.Load(),
		BadPackets:      s.stats.badPackets.Load(),
	}
}

type pendingQuery struct {
	addr netip.AddrPort
	ch   chan msg
}

// New creates a server listening on cfg.Addr. Call Run to start
// answering and receiving replies.
func New(cfg Config) (*Server, error) {
	if cfg.ID == (id.Hash{}) {
		cfg.ID = id.Random()
	}
	if cfg.QueryTimeout <= 0 {
		cfg.QueryTimeout = defaultQueryTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}

	laddr, err := net.ResolveUDPAddr("udp4", cfg.Addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp4", laddr)
	if err != nil {
		return nil, err
	}

	s := &Server{
		id:      cfg.ID,
		conn:    conn,
		table:   newTable(cfg.ID),
		timeout: cfg.QueryTimeout,
		log:     cfg.Logger,
		pending: map[string]*pendingQuery{},
		peers:   map[id.Hash]map[netip.AddrPort]time.Time{},
	}
	rand.Read(s.secret[:])
	s.prevSecret = s.secret

	return s, nil
}

// ID returns our node ID.
func (s *Server) ID() id.Hash {
	return s.id
}

// Addr returns the address the server is listening on.
func (s *Server) Addr() netip.AddrPort {
	return s.conn.LocalAddr().(*net.UDPAddr).AddrPort()
}

// Nodes returns the number of nodes in the routing table.
func (s *Server) Nodes() int {
	return s.table.len()
}

// Run reads packets until ctx is cancelled, then closes the socket.
func (s *Server) Run(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() { s.conn.Close() })
	defer stop()

	go s.maintain(ctx)

	buf := make([]byte, 65535)
	for {
		n, from, err := s.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		from = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())

		m, err := decodeMsg(buf[:n])
		if err != nil {
			s.stats.badPackets.Add(1)
			s.log.Debug("dht bad packet", "from", from, "len", n, "err", err)
			continue
		}

		switch m.Y {
		case "q":
			s.handleQuery(m, from)
		case "r", "e":
			s.handleReply(m, from)
		}
	}
}

// maintain rotates token secrets and expires stored peers.
func (s *Server) maintain(ctx context.Context) {
	ticker := time.NewTicker(tokenRotation)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		s.tokenMu.Lock()
		s.prevSecret = s.secret
		rand.Read(s.secret[:])
		s.tokenMu.Unlock()

		now := time.Now()
		s.peersMu.Lock()
		for h, set := range s.peers {
			for a, seen := range set {
				if now.Sub(seen) > peerTTL {
					delete(set, a)
				}
			}
			if len(set) == 0 {
				delete(s.peers, h)
			}
		}
		s.peersMu.Unlock()
	}
}

func (s *Server) send(b []byte, to netip.AddrPort) {
	if _, err := s.conn.WriteToUDPAddrPort(b, to); err != nil {
		s.log.Debug("send failed", "to", to, "err", err)
	}
}

// query sends a query to addr and waits for the reply. A valid reply
// adds the responder to the routing table.
func (s *Server) query(ctx context.Context, addr netip.AddrPort, method string, args map[string]string) (map[string]string, id.Hash, error) {
	args["id"] = bencoding.EncodeString(s.id.String())

	s.mu.Lock()
	s.nextTx++
	tx := string(binary.BigEndian.AppendUint16(nil, s.nextTx))
	p := &pendingQuery{addr: addr, ch: make(chan msg, 1)}
	s.pending[tx] = p
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.pending, tx)
		s.mu.Unlock()
	}()

	start := time.Now()
	s.stats.sent.Add(1)
	s.send(encodeQuery(tx, method, args), addr)

	timer := time.NewTimer(s.timeout)
	defer timer.Stop()

	var m msg
	select {
	case m = <-p.ch:
	case <-timer.C:
		s.stats.timeouts.Add(1)
		s.log.Debug("dht query", "method", method, "to", addr, "result", "timeout", "dur", time.Since(start))
		return nil, id.Hash{}, fmt.Errorf("%s to %s timed out", method, addr)
	case <-ctx.Done():
		return nil, id.Hash{}, ctx.Err()
	}

	if m.Y == "e" {
		s.stats.errorReplies.Add(1)
		s.log.Debug("dht query", "method", method, "to", addr, "result", "error", "code", m.ErrCode, "msg", m.ErrMsg, "dur", time.Since(start))
		return nil, id.Hash{}, fmt.Errorf("%s to %s: error %d: %s", method, addr, m.ErrCode, m.ErrMsg)
	}
	nodeID, ok := getID(m.R, "id")
	if !ok {
		s.stats.errorReplies.Add(1)
		s.log.Debug("dht query", "method", method, "to", addr, "result", "error", "msg", "reply has no node ID", "dur", time.Since(start))
		return nil, id.Hash{}, fmt.Errorf("%s to %s: reply has no node ID", method, addr)
	}
	s.stats.replies.Add(1)
	s.log.Debug("dht query", "method", method, "to", addr, "result", "ok", "dur", time.Since(start))
	s.table.insert(node{ID: nodeID, Addr: addr})

	return m.R, nodeID, nil
}

// queryNode is query for a node we know the ID of, so an unanswered
// query counts against it in the routing table.
func (s *Server) queryNode(ctx context.Context, n node, method string, args map[string]string) (map[string]string, error) {
	r, _, err := s.query(ctx, n.Addr, method, args)
	if err != nil && ctx.Err() == nil {
		s.table.failed(n.ID)
	}
	return r, err
}

func (s *Server) handleReply(m msg, from netip.AddrPort) {
	s.mu.Lock()
	p, ok := s.pending[m.T]
	s.mu.Unlock()

	if !ok || p.addr != from {
		return
	}
	select {
	case p.ch <- m:
	default:
	}
}

// Ping checks that a node is alive and adds it to the routing table.
func (s *Server) Ping(ctx context.Context, addr netip.AddrPort) (id.Hash, error) {
	_, nodeID, err := s.query(ctx, addr, "ping", map[string]string{})
	return nodeID, err
}

// findNode asks addr for the nodes it knows closest to target.
func (s *Server) findNode(ctx context.Context, addr netip.AddrPort, target id.Hash) ([]node, error) {
	r, _, err := s.query(ctx, addr, "find_node", map[string]string{
		"target": bencoding.EncodeString(target.String()),
	})
	if err != nil {
		return nil, err
	}
	nodes, _ := getString(r, "nodes")
	return decodeNodes(nodes), nil
}

type getPeersResult struct {
	peers []netip.AddrPort
	nodes []node
	token string
}

func (s *Server) getPeers(ctx context.Context, n node, infoHash id.Hash) (getPeersResult, error) {
	r, err := s.queryNode(ctx, n, "get_peers", map[string]string{
		"info_hash": bencoding.EncodeString(infoHash.String()),
	})
	if err != nil {
		return getPeersResult{}, err
	}

	var res getPeersResult
	res.token, _ = getString(r, "token")
	if nodes, ok := getString(r, "nodes"); ok {
		res.nodes = decodeNodes(nodes)
	}
	if values, ok := r["values"]; ok {
		res.peers = decodeValues(values)
	}
	return res, nil
}

func (s *Server) announcePeer(ctx context.Context, n node, infoHash id.Hash, port int, token string) error {
	_, err := s.queryNode(ctx, n, "announce_peer", map[string]string{
		"info_hash": bencoding.EncodeString(infoHash.String()),
		"port":      bencoding.EncodeInteger(port),
		"token":     bencoding.EncodeString(token),
	})
	return err
}

func (s *Server) handleQuery(m msg, from netip.AddrPort) {
	s.stats.received.Add(1)
	s.log.Debug("dht query received", "method", m.Q, "from", from)

	sender, ok := getID(m.A, "id")
	if !ok {
		s.send(encodeError(m.T, errProtocol, "missing id"), from)
		return
	}
	s.table.insert(node{ID: sender, Addr: from})

	r := map[string]string{"id": bencoding.EncodeString(s.id.String())}

	switch m.Q {
	case "ping":

	case "find_node":
		target, ok := getID(m.A, "target")
		if !ok {
			s.send(encodeError(m.T, errProtocol, "missing target"), from)
			return
		}
		r["nodes"] = bencoding.EncodeString(encodeNodes(s.table.closest(target, K)))

	case "get_peers":
		infoHash, ok := getID(m.A, "info_hash")
		if !ok {
			s.send(encodeError(m.T, errProtocol, "missing info_hash"), from)
			return
		}
		r["token"] = bencoding.EncodeString(s.token(from.Addr(), s.currentSecret()))
		r["nodes"] = bencoding.EncodeString(encodeNodes(s.table.closest(infoHash, K)))
		if peers := s.storedPeers(infoHash); len(peers) > 0 {
			var values []string
			for _, p := range peers {
				values = append(values, bencoding.EncodeString(encodePeer(p)))
			}
			r["values"] = bencoding.EncodeList(values...)
		}

	case "announce_peer":
		infoHash, ok := getID(m.A, "info_hash")
		if !ok {
			s.send(encodeError(m.T, errProtocol, "missing info_hash"), from)
			return
		}
		token, _ := getString(m.A, "token")
		if !s.validToken(from.Addr(), token) {
			s.send(encodeError(m.T, errProtocol, "bad token"), from)
			return
		}
		port, _ := getInt(m.A, "port")
		if implied, _ := getInt(m.A, "implied_port"); implied != 0 {
			port = int(from.Port())
		}
		if port <= 0 || port > 65535 {
			s.send(encodeError(m.T, errProtocol, "bad port"), from)
			return
		}
		s.storePeer(infoHash, netip.AddrPortFrom(from.Addr(), uint16(port)))
		s.stats.announced.Add(1)
		s.log.Debug("dht peer announced", "infohash", infoHash.Hex(), "peer", netip.AddrPortFrom(from.Addr(), uint16(port)))

	default:
		s.send(encodeError(m.T, errMethod, "Method Unknown"), from)
		return
	}

	s.send(encodeResponse(m.T, r), from)
}

func (s *Server) currentSecret() [16]byte {
	s.tokenMu.Lock()
	defer s.tokenMu.Unlock()
	return s.secret
}

// token binds a get_peers reply to the requester's IP, so only it can
// announce with it. Tokens from the previous secret stay valid for one
// rotation.
func (s *Server) token(ip netip.Addr, secret [16]byte) string {
	b := ip.As16()
	h := sha1.Sum(append(secret[:], b[:]...))
	return string(h[:8])
}

func (s *Server) validToken(ip netip.Addr, token string) bool {
	s.tokenMu.Lock()
	cur, prev := s.secret, s.prevSecret
	s.tokenMu.Unlock()
	return token != "" && (token == s.token(ip, cur) || token == s.token(ip, prev))
}

func (s *Server) storePeer(infoHash id.Hash, addr netip.AddrPort) {
	s.peersMu.Lock()
	defer s.peersMu.Unlock()

	set := s.peers[infoHash]
	if set == nil {
		set = map[netip.AddrPort]time.Time{}
		s.peers[infoHash] = set
	}
	if _, ok := set[addr]; !ok && len(set) >= maxPeersPerHash {
		return
	}
	set[addr] = time.Now()
}

func (s *Server) storedPeers(infoHash id.Hash) []netip.AddrPort {
	s.peersMu.Lock()
	defer s.peersMu.Unlock()

	var peers []netip.AddrPort
	for a := range s.peers[infoHash] {
		if len(peers) == maxValues {
			break
		}
		peers = append(peers, a)
	}
	return peers
}
