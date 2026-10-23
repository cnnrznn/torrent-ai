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
}

// Stats is a snapshot of a session's progress.
type Stats struct {
	Pieces     int
	Have       int
	Left       int64
	Peers      int
	Downloaded int64
	Uploaded   int64
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

	complete     chan struct{}
	completeOnce sync.Once

	statsMu sync.Mutex
	stats   Stats
}

type peer struct {
	conn *peerwire.Conn
	addr netip.AddrPort
	have peerwire.Bitfield

	amChoking      bool
	amInterested   bool
	peerChoking    bool
	peerInterested bool

	requests map[block]time.Time
}

type event any

type peerConnected struct {
	conn *peerwire.Conn
	addr netip.AddrPort
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
	}

	if existed {
		if err := s.verifyExisting(); err != nil {
			storage.Close()
			return nil, err
		}
	}
	if s.picker.complete() {
		s.markComplete()
	}
	s.updateStats()

	return s, nil
}

func (s *Session) verifyExisting() error {
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
	s.log.Info("verified existing data", "have", s.picker.have.Count(), "pieces", s.m.NumPieces())
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
	return peerwire.NewHandshake(s.m.InfoHash(), s.cfg.PeerID, false)
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
		go func() {
			c, err := peerwire.Dial(ctx, addr, s.handshake())
			if err != nil {
				s.log.Debug("dial failed", "addr", addr, "err", err)
				s.swarm.Release(addr, true)
				return
			}
			if !s.send(ctx, peerConnected{c, addr}) {
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
				s.swarm.Release(addr, true)
				return
			}
			if !s.send(ctx, peerConnected{c, addr}) {
				c.Close()
				s.swarm.Release(addr, false)
			}
		}()
	}
}

func (s *Session) handle(ctx context.Context, ev event) {
	switch ev := ev.(type) {
	case peerConnected:
		s.addPeer(ctx, ev.conn, ev.addr)
	case peerMessage:
		if _, ok := s.peers[ev.p]; !ok {
			return
		}
		if err := s.handleMessage(ev.p, ev.msg); err != nil {
			s.log.Debug("bad message", "addr", ev.p.addr, "msg", ev.msg.ID, "err", err)
			ev.p.conn.Close()
		}
	case peerClosed:
		s.removePeer(ev.p)
	}
}

func (s *Session) addPeer(ctx context.Context, c *peerwire.Conn, addr netip.AddrPort) {
	p := &peer{
		conn:        c,
		addr:        addr,
		have:        peerwire.NewBitfield(s.m.NumPieces()),
		amChoking:   true,
		peerChoking: true,
		requests:    map[block]time.Time{},
	}
	s.peers[p] = struct{}{}
	s.log.Debug("peer connected", "addr", addr, "peers", len(s.peers))

	go func() {
		for m := range c.Messages() {
			if !s.send(ctx, peerMessage{p, m}) {
				return
			}
		}
		s.send(ctx, peerClosed{p})
	}()

	if s.picker.have.Count() > 0 {
		c.Send(peerwire.MsgBitfield(s.picker.have))
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
	s.log.Debug("peer closed", "addr", p.addr, "err", p.conn.Err(), "peers", len(s.peers))
}

func (s *Session) handleMessage(p *peer, m peerwire.Message) error {
	switch m.ID {
	case peerwire.Choke:
		p.peerChoking = true
		// A choke discards all our pending requests.
		for b := range p.requests {
			s.picker.released(b)
		}
		clear(p.requests)

	case peerwire.Unchoke:
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
	}

	return nil
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
	ok := sha1.Sum(data) == s.m.PieceHash(i)
	if !ok {
		s.log.Warn("piece failed hash check", "piece", i)
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
		s.log.Info("download complete", "name", s.m.Name())
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
		expired := false
		for b, sentAt := range p.requests {
			if now.Sub(sentAt) > requestTimeout {
				delete(p.requests, b)
				s.picker.released(b)
				expired = true
			}
		}
		if expired {
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

	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	s.stats = Stats{
		Pieces:     s.m.NumPieces(),
		Have:       s.picker.have.Count(),
		Left:       left,
		Peers:      len(s.peers),
		Downloaded: s.downloaded,
		Uploaded:   s.uploaded,
	}
}
