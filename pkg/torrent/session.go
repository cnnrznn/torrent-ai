package torrent

import (
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cnnrznn/torrent-scratch/pkg/id"
	"github.com/cnnrznn/torrent-scratch/pkg/metainfo"
	"github.com/cnnrznn/torrent-scratch/pkg/peerwire"
	"github.com/cnnrznn/torrent-scratch/pkg/swarm"
)

const (
	defaultMaxInFlight = 16
	requestTimeout     = time.Minute
	maxRequestLength   = 128 * 1024
)

// Config configures a Session.
type Config struct {
	// Dir is the directory the torrent's files are written under.
	Dir string
	// PeerID identifies us to other peers.
	PeerID id.Hash
	// Listener, if set, accepts incoming peer connections. Run closes
	// it when it returns.
	Listener net.Listener
	// MaxInFlight is the number of block requests kept outstanding per
	// peer. Defaults to 16.
	MaxInFlight int
	// Logger receives debug and progress logs. Defaults to discarding.
	Logger *slog.Logger
	// DHTPort, if set, is the UDP port of our DHT node. It is
	// advertised in the handshake and sent to peers that run the DHT.
	DHTPort int
	// OnPort is called with a peer's DHT node address when it sends a
	// port message.
	OnPort func(netip.AddrPort)
}

// Stats is a snapshot of a session's progress.
type Stats struct {
	Pieces     int
	Have       int
	Left       int64
	Downloaded int64
	Uploaded   int64

	Peers      int // connected peers
	UnchokedUs int // peers that let us request from them
	Interested int // peers that have pieces we want
	Requests   int // block requests in flight

	Dials           int64 // outgoing connection attempts
	DialFailures    int64 // outgoing attempts that failed
	Accepted        int64 // incoming connections that completed a handshake
	HashFailures    int64 // pieces that failed their SHA-1 check
	RequestTimeouts int64 // block requests that went unanswered
}

// Session downloads and seeds one torrent. A single goroutine (Run)
// owns all of its state; connections talk to it through events.
type Session struct {
	m       *metainfo.Metainfo
	swarm   *swarm.Swarm
	cfg     Config
	log     *slog.Logger
	storage *Storage
	picker  *picker

	peers      map[*peer]struct{}
	events     chan event
	downloaded int64
	uploaded   int64
	started    time.Time

	// sources records which peers sent blocks of each partial piece,
	// so a hash failure can name its suspects.
	sources map[int]map[netip.AddrPort]bool

	dials, dialFailures, accepted atomic.Int64
	hashFailures, timeouts        int64

	complete     chan struct{}
	completeOnce sync.Once

	statsMu sync.Mutex
	stats   Stats
}

type peer struct {
	conn *peerwire.Conn
	addr netip.AddrPort
	have peerwire.Bitfield

	incoming    bool
	client      string
	connectedAt time.Time
	downloaded  int64
	uploaded    int64

	amChoking      bool
	amInterested   bool
	peerChoking    bool
	peerInterested bool

	requests map[block]time.Time

	// utMetadata is the peer's extended message ID for ut_metadata, or
	// 0 if it doesn't support it.
	utMetadata int
}

type event any

type peerConnected struct {
	conn     *peerwire.Conn
	addr     netip.AddrPort
	incoming bool
}

type peerMessage struct {
	p   *peer
	msg peerwire.Message
}

type peerClosed struct {
	p *peer
}

// New opens the torrent's files under cfg.Dir. Files that already
// exist are verified, so an interrupted download resumes and a
// complete one seeds.
func New(m *metainfo.Metainfo, sw *swarm.Swarm, cfg Config) (*Session, error) {
	if cfg.MaxInFlight <= 0 {
		cfg.MaxInFlight = defaultMaxInFlight
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}

	storage, existed, err := OpenStorage(cfg.Dir, m)
	if err != nil {
		return nil, err
	}

	s := &Session{
		m:        m,
		swarm:    sw,
		cfg:      cfg,
		log:      cfg.Logger,
		storage:  storage,
		picker:   newPicker(m, peerwire.NewBitfield(m.NumPieces())),
		peers:    map[*peer]struct{}{},
		events:   make(chan event),
		complete: make(chan struct{}),
		sources:  map[int]map[netip.AddrPort]bool{},
	}

	if existed {
		if err := s.verifyExisting(); err != nil {
			storage.Close()
			return nil, err
		}
	}
	s.updateStats()
	st := s.Stats()
	s.log.Info("session ready", "name", m.Name(), "pieces", st.Pieces, "have", st.Have, "left", st.Left, "resumed", existed)
	if s.picker.complete() {
		s.markComplete()
	}

	return s, nil
}

func (s *Session) verifyExisting() error {
	start := time.Now()
	buf := make([]byte, s.m.PieceLength())
	for i := range s.m.NumPieces() {
		b := buf[:s.m.PieceSize(i)]
		if err := s.storage.ReadAt(b, int64(i)*int64(s.m.PieceLength())); err != nil {
			return fmt.Errorf("verify piece %d: %w", i, err)
		}
		if sha1.Sum(b) == s.m.PieceHash(i) {
			s.picker.have.Set(i)
		}
	}
	s.log.Info("verified existing data", "have", s.picker.have.Count(), "pieces", s.m.NumPieces(), "dur", time.Since(start))
	return nil
}

// Complete is closed once every piece has been downloaded and
// verified.
func (s *Session) Complete() <-chan struct{} {
	return s.complete
}

// Stats returns a snapshot of the session's progress.
func (s *Session) Stats() Stats {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	return s.stats
}

// Run downloads and seeds until ctx is cancelled.
func (s *Session) Run(ctx context.Context) error {
	s.started = time.Now()
	defer s.storage.Close()
	defer func() {
		for p := range s.peers {
			p.conn.Close()
			s.swarm.Release(p.addr, false)
		}
	}()

	if ln := s.cfg.Listener; ln != nil {
		stop := context.AfterFunc(ctx, func() { ln.Close() })
		defer stop()
		go s.acceptLoop(ctx, ln)
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	s.connectPeers(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev := <-s.events:
			s.handle(ctx, ev)
		case <-s.swarm.Notify():
			s.connectPeers(ctx)
		case <-ticker.C:
			s.expireRequests()
			s.connectPeers(ctx)
			s.updateStats()
		}
	}
}

// send delivers an event to Run, giving up if ctx is cancelled.
func (s *Session) send(ctx context.Context, ev event) bool {
	select {
	case s.events <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *Session) handshake() peerwire.Handshake {
	return peerwire.NewHandshake(s.m.InfoHash(), s.cfg.PeerID, s.cfg.DHTPort != 0)
}

// connectPeers dials addresses from the swarm until it runs out of
// addresses or free slots. Once complete we only accept connections.
func (s *Session) connectPeers(ctx context.Context) {
	if s.picker.complete() {
		return
	}
	for {
		addr, ok := s.swarm.Next()
		if !ok {
			return
		}
		s.dials.Add(1)
		go func() {
			c, err := peerwire.Dial(ctx, addr, s.handshake())
			if err != nil {
				s.dialFailures.Add(1)
				s.log.Debug("peer dial failed", "addr", addr, "err", err)
				s.swarm.Release(addr, true)
				return
			}
			if !s.send(ctx, peerConnected{c, addr, false}) {
				c.Close()
				s.swarm.Release(addr, false)
			}
		}()
	}
}

func (s *Session) acceptLoop(ctx context.Context, ln net.Listener) {
	for {
		nc, err := ln.Accept()
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("accept failed", "err", err)
			}
			return
		}

		addr := nc.RemoteAddr().(*net.TCPAddr).AddrPort()
		if !s.swarm.Acquire(addr) {
			nc.Close()
			continue
		}
		go func() {
			c, err := peerwire.Accept(nc, s.handshake())
			if err != nil {
				s.log.Debug("peer accept failed", "addr", addr, "err", err)
				s.swarm.Release(addr, true)
				return
			}
			s.accepted.Add(1)
			if !s.send(ctx, peerConnected{c, addr, true}) {
				c.Close()
				s.swarm.Release(addr, false)
			}
		}()
	}
}

func (s *Session) handle(ctx context.Context, ev event) {
	switch ev := ev.(type) {
	case peerConnected:
		s.addPeer(ctx, ev.conn, ev.addr, ev.incoming)
	case peerMessage:
		if _, ok := s.peers[ev.p]; !ok {
			return
		}
		if err := s.handleMessage(ev.p, ev.msg); err != nil {
			s.log.Debug("peer protocol error", "addr", ev.p.addr, "client", ev.p.client, "msg", ev.msg.ID, "err", err)
			ev.p.conn.Close()
		}
	case peerClosed:
		s.removePeer(ev.p)
	}
}

func (s *Session) addPeer(ctx context.Context, c *peerwire.Conn, addr netip.AddrPort, incoming bool) {
	p := &peer{
		conn:        c,
		addr:        addr,
		have:        peerwire.NewBitfield(s.m.NumPieces()),
		incoming:    incoming,
		client:      peerwire.ClientName(c.Peer.PeerID),
		connectedAt: time.Now(),
		amChoking:   true,
		peerChoking: true,
		requests:    map[block]time.Time{},
	}
	s.peers[p] = struct{}{}
	s.log.Debug("peer connected",
		"addr", addr,
		"incoming", incoming,
		"client", p.client,
		"extensions", c.Peer.SupportsExtensions(),
		"dht", c.Peer.SupportsDHT(),
		"peers", len(s.peers),
	)

	go func() {
		for m := range c.Messages() {
			if !s.send(ctx, peerMessage{p, m}) {
				return
			}
		}
		s.send(ctx, peerClosed{p})
	}()

	if c.Peer.SupportsExtensions() {
		hs := peerwire.ExtHandshake{
			M:            map[string]int{peerwire.UTMetadata: peerwire.UTMetadataID},
			MetadataSize: len(s.m.RawInfo()),
		}
		c.Send(peerwire.MsgExtended(peerwire.ExtHandshakeID, hs.Encode()))
	}
	if s.picker.have.Count() > 0 {
		c.Send(peerwire.MsgBitfield(s.picker.have))
	}
	if s.cfg.DHTPort != 0 && c.Peer.SupportsDHT() {
		c.Send(peerwire.MsgPort(uint16(s.cfg.DHTPort)))
	}
}

func (s *Session) removePeer(p *peer) {
	if _, ok := s.peers[p]; !ok {
		return
	}
	delete(s.peers, p)

	for b := range p.requests {
		s.picker.released(b)
	}
	s.picker.peerLeft(p.have)
	s.swarm.Release(p.addr, true)
	s.log.Debug("peer closed",
		"addr", p.addr,
		"incoming", p.incoming,
		"client", p.client,
		"err", p.conn.Err(),
		"connected", time.Since(p.connectedAt).Round(time.Millisecond),
		"down", p.downloaded,
		"up", p.uploaded,
		"had", p.have.Count(),
		"peers", len(s.peers),
	)
}

func (s *Session) handleMessage(p *peer, m peerwire.Message) error {
	switch m.ID {
	case peerwire.Choke:
		if !p.peerChoking {
			s.log.Debug("peer choked us", "addr", p.addr, "client", p.client, "dropped_requests", len(p.requests))
		}
		p.peerChoking = true
		// A choke discards all our pending requests.
		for b := range p.requests {
			s.picker.released(b)
		}
		clear(p.requests)

	case peerwire.Unchoke:
		if p.peerChoking {
			s.log.Debug("peer unchoked us", "addr", p.addr, "client", p.client)
		}
		p.peerChoking = false
		s.fillRequests(p)

	case peerwire.Interested:
		p.peerInterested = true
		// We unchoke everyone who asks.
		if p.amChoking {
			p.amChoking = false
			p.conn.Send(peerwire.Message{ID: peerwire.Unchoke})
		}

	case peerwire.NotInterested:
		p.peerInterested = false

	case peerwire.Have:
		i, err := m.ParseHave()
		if err != nil {
			return err
		}
		if i >= s.m.NumPieces() {
			return fmt.Errorf("have for piece %d out of range", i)
		}
		if !p.have.Has(i) {
			p.have.Set(i)
			s.picker.peerHas(i)
		}
		s.updateInterest(p)
		s.fillRequests(p)

	case peerwire.BitfieldMsg:
		bf, err := peerwire.ParseBitfield(m.Payload, s.m.NumPieces())
		if err != nil {
			return err
		}
		s.picker.peerLeft(p.have)
		p.have = bf
		s.log.Debug("peer bitfield", "addr", p.addr, "client", p.client, "has", bf.Count(), "pieces", s.m.NumPieces())
		for i := range s.m.NumPieces() {
			if bf.Has(i) {
				s.picker.peerHas(i)
			}
		}
		s.updateInterest(p)
		s.fillRequests(p)

	case peerwire.Request:
		index, begin, length, err := m.ParseRequest()
		if err != nil {
			return err
		}
		return s.serveRequest(p, index, begin, length)

	case peerwire.Piece:
		index, begin, data, err := m.ParsePiece()
		if err != nil {
			return err
		}
		s.receiveBlock(p, block{index, begin, len(data)}, data)

	case peerwire.Port:
		port, err := m.ParsePort()
		if err != nil {
			return err
		}
		if s.cfg.OnPort != nil && port != 0 {
			s.cfg.OnPort(netip.AddrPortFrom(p.addr.Addr(), port))
		}

	case peerwire.Extended:
		extID, payload, err := m.ParseExtended()
		if err != nil {
			return err
		}
		return s.handleExtended(p, extID, payload)
	}

	return nil
}

func (s *Session) handleExtended(p *peer, extID byte, payload []byte) error {
	switch extID {
	case peerwire.ExtHandshakeID:
		h, err := peerwire.ParseExtHandshake(payload)
		if err != nil {
			return err
		}
		p.utMetadata = h.M[peerwire.UTMetadata]
		s.log.Debug("peer extensions", "addr", p.addr, "client", p.client, "v", h.V, "ut_metadata", p.utMetadata, "metadata_size", h.MetadataSize)

	case peerwire.UTMetadataID:
		req, err := peerwire.ParseMetadataMsg(payload)
		if err != nil {
			return err
		}
		if req.Type != peerwire.MetadataRequest || p.utMetadata == 0 {
			return nil
		}
		s.serveMetadata(p, req.Piece)
	}
	return nil
}

// serveMetadata answers a ut_metadata request with a piece of our info
// dict, or a reject if the piece doesn't exist.
func (s *Session) serveMetadata(p *peer, piece int) {
	info := s.m.RawInfo()
	begin := piece * peerwire.MetadataPieceSize

	res := peerwire.MetadataMsg{Type: peerwire.MetadataReject, Piece: piece}
	if piece >= 0 && begin < len(info) {
		res = peerwire.MetadataMsg{
			Type:      peerwire.MetadataData,
			Piece:     piece,
			TotalSize: len(info),
			Data:      []byte(info[begin:min(begin+peerwire.MetadataPieceSize, len(info))]),
		}
	}
	p.conn.Send(peerwire.MsgExtended(byte(p.utMetadata), res.Encode()))
}

func (s *Session) serveRequest(p *peer, index, begin, length int) error {
	if p.amChoking {
		return nil
	}
	if index < 0 || index >= s.m.NumPieces() || !s.picker.have.Has(index) {
		return nil
	}
	if length <= 0 || length > maxRequestLength || begin < 0 || begin+length > s.m.PieceSize(index) {
		return errors.New("invalid request")
	}

	data := make([]byte, length)
	off := int64(index)*int64(s.m.PieceLength()) + int64(begin)
	if err := s.storage.ReadAt(data, off); err != nil {
		s.log.Error("read failed", "piece", index, "err", err)
		return nil
	}
	p.conn.Send(peerwire.MsgPiece(index, begin, data))
	s.uploaded += int64(length)
	p.uploaded += int64(length)
	return nil
}

func (s *Session) receiveBlock(p *peer, b block, data []byte) {
	if _, ok := p.requests[b]; !ok {
		// Unrequested, or already cancelled.
		return
	}
	delete(p.requests, b)

	isNew, complete := s.picker.received(b, data)
	if isNew {
		s.downloaded += int64(len(data))
		p.downloaded += int64(len(data))
		if s.sources[b.piece] == nil {
			s.sources[b.piece] = map[netip.AddrPort]bool{}
		}
		s.sources[b.piece][p.addr] = true
	}

	// Endgame: cancel the same block at other peers.
	for q := range s.peers {
		if _, ok := q.requests[b]; ok && q != p {
			delete(q.requests, b)
			s.picker.released(b)
			q.conn.Send(peerwire.MsgCancel(b.piece, b.begin, b.length))
		}
	}

	if complete {
		s.finishPiece(b.piece)
	}
	s.fillRequests(p)
}

func (s *Session) finishPiece(i int) {
	data := s.picker.pieceData(i)
	sources := s.sources[i]
	delete(s.sources, i)

	ok := sha1.Sum(data) == s.m.PieceHash(i)
	if !ok {
		s.hashFailures++
		var from []string
		for a := range sources {
			from = append(from, a.String())
		}
		s.log.Warn("piece hash failed", "piece", i, "from", from)
	} else if err := s.storage.WriteAt(data, int64(i)*int64(s.m.PieceLength())); err != nil {
		s.log.Error("write failed", "piece", i, "err", err)
		ok = false
	}
	s.picker.finish(i, ok)
	if !ok {
		return
	}

	for q := range s.peers {
		q.conn.Send(peerwire.MsgHave(i))
		s.updateInterest(q)
	}

	if s.picker.complete() {
		s.markComplete()
		s.updateStats()
	}
}

func (s *Session) markComplete() {
	s.completeOnce.Do(func() {
		attrs := []any{"name", s.m.Name(), "downloaded", s.downloaded, "hash_failures", s.hashFailures}
		if !s.started.IsZero() {
			dur := time.Since(s.started)
			attrs = append(attrs, "dur", dur.Round(time.Millisecond), "avg_rate", int64(float64(s.downloaded)/dur.Seconds()))
		}
		s.log.Info("download complete", attrs...)
		close(s.complete)
	})
}

func (s *Session) updateInterest(p *peer) {
	want := s.picker.wants(p.have)
	if want == p.amInterested {
		return
	}
	p.amInterested = want
	if want {
		p.conn.Send(peerwire.Message{ID: peerwire.Interested})
	} else {
		p.conn.Send(peerwire.Message{ID: peerwire.NotInterested})
	}
}

func (s *Session) fillRequests(p *peer) {
	if p.peerChoking || !p.amInterested {
		return
	}

	sent := func(b block) bool {
		_, ok := p.requests[b]
		return ok
	}
	now := time.Now()
	for len(p.requests) < s.cfg.MaxInFlight {
		b, ok := s.picker.next(p.have, sent)
		if !ok {
			return
		}
		s.picker.requested(b)
		p.requests[b] = now
		p.conn.Send(peerwire.MsgRequest(b.piece, b.begin, b.length))
	}
}

// expireRequests gives up on requests a peer hasn't answered, so the
// blocks can be fetched elsewhere.
func (s *Session) expireRequests() {
	now := time.Now()
	for p := range s.peers {
		expired := 0
		for b, sentAt := range p.requests {
			if now.Sub(sentAt) > requestTimeout {
				delete(p.requests, b)
				s.picker.released(b)
				expired++
			}
		}
		if expired > 0 {
			s.timeouts += int64(expired)
			s.log.Debug("peer requests timed out", "addr", p.addr, "client", p.client, "expired", expired, "choked", p.peerChoking)
			s.fillRequests(p)
		}
	}
}

func (s *Session) updateStats() {
	left := int64(0)
	for i := range s.m.NumPieces() {
		if !s.picker.have.Has(i) {
			left += int64(s.m.PieceSize(i))
		}
	}

	var unchoked, interested, requests int
	for p := range s.peers {
		if !p.peerChoking {
			unchoked++
		}
		if p.amInterested {
			interested++
		}
		requests += len(p.requests)
	}

	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	s.stats = Stats{
		Pieces:          s.m.NumPieces(),
		Have:            s.picker.have.Count(),
		Left:            left,
		Downloaded:      s.downloaded,
		Uploaded:        s.uploaded,
		Peers:           len(s.peers),
		UnchokedUs:      unchoked,
		Interested:      interested,
		Requests:        requests,
		Dials:           s.dials.Load(),
		DialFailures:    s.dialFailures.Load(),
		Accepted:        s.accepted.Load(),
		HashFailures:    s.hashFailures,
		RequestTimeouts: s.timeouts,
	}
}
