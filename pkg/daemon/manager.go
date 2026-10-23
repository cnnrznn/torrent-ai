// Package daemon runs many torrents in one process: they share a
// listener, a DHT node, and caps on peer connections and memory.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/cnnrznn/torrent-ai/pkg/dht"
	"github.com/cnnrznn/torrent-ai/pkg/id"
	"github.com/cnnrznn/torrent-ai/pkg/metainfo"
	"github.com/cnnrznn/torrent-ai/pkg/peersource"
	"github.com/cnnrznn/torrent-ai/pkg/peerwire"
	"github.com/cnnrznn/torrent-ai/pkg/swarm"
	"github.com/cnnrznn/torrent-ai/pkg/torrent"
	"github.com/cnnrznn/torrent-ai/pkg/tracker"
)

// Config configures a Manager.
type Config struct {
	// PeerID identifies us to peers, for every torrent.
	PeerID id.Hash
	// Listener accepts incoming peer connections for all torrents. Run
	// closes it when it returns.
	Listener net.Listener
	// Port is the peer port announced to trackers and the DHT; usually
	// the Listener's.
	Port int
	// Logger receives logs; each torrent's lines carry its infohash.
	Logger *slog.Logger

	// DHT, if set, is a running DHT node shared by every torrent.
	DHT       *dht.Server
	DHTSource peersource.DHTConfig

	// UseTrackers announces to each torrent's own trackers.
	UseTrackers bool
	// TrackerOptions apply to every tracker announce.
	TrackerOptions []tracker.Option
	// NumWant is how many peers to ask each tracker for; 0 leaves it
	// to the tracker.
	NumWant int
	// TrackerLoop tunes the re-announce schedule.
	TrackerLoop peersource.TrackerLoopConfig

	// SeedRatio stops seeding a torrent once it has uploaded this many
	// times its size; 0 means no limit.
	SeedRatio float64
	// SeedTime stops seeding a torrent this long after it completed;
	// 0 means no limit.
	SeedTime time.Duration

	// MaxPeers caps peer connections across all torrents; 0 means no
	// shared cap. Swarm.MaxPeers still caps each torrent.
	MaxPeers int
	// MaxMemory caps memory held in partial pieces across all
	// torrents; 0 means no limit.
	MaxMemory int64

	// Store, if set, persists torrents and their info dicts so Restore
	// can bring them back after a restart.
	Store *Store

	// Swarm, Session and Metadata are templates for each torrent.
	// Session's Dir, PeerID, Listener, Logger, DHTPort, OnPort, Memory
	// and MaxMemory are set per torrent.
	Swarm    swarm.Config
	Session  torrent.Config
	Metadata torrent.MetadataConfig
}

// Source is a torrent to add.
type Source struct {
	// Magnet is a magnet link; set it or Torrent.
	Magnet string
	// Torrent is the contents of a .torrent file.
	Torrent []byte
	// Dir is where the torrent's files go.
	Dir string
	// Peers are addresses to try in addition to trackers and the DHT.
	Peers []netip.AddrPort
	// Trackers are announce URLs in addition to the torrent's own.
	Trackers []string

	// restoredInfo is a magnet's cached info dict, set by Restore.
	restoredInfo string
	// finished marks a torrent restored after its seeding limit; it
	// isn't started again.
	finished bool
}

// State is where a torrent is in its life.
type State string

const (
	StateMetadata    State = "metadata"    // fetching the info dict
	StateVerifying   State = "verifying"   // checking data already on disk
	StateDownloading State = "downloading" // downloading pieces
	StateSeeding     State = "seeding"     // complete, serving others
	StateFinished    State = "finished"    // complete, seeding limit reached
	StateFailed      State = "failed"      // stopped by an error; see Status.Error
	StateStopped     State = "stopped"     // removed or shut down
)

// Status describes one torrent.
type Status struct {
	InfoHash string    `json:"infohash"`
	Name     string    `json:"name,omitempty"`
	State    State     `json:"state"`
	Dir      string    `json:"dir"`
	AddedAt  time.Time `json:"added_at"`
	Error    string    `json:"error,omitempty"`

	// Size and progress; zero until the metadata is known.
	Size       int64 `json:"size"`
	Pieces     int   `json:"pieces"`
	Have       int   `json:"have"`
	Left       int64 `json:"left"`
	Downloaded int64 `json:"downloaded"`
	Uploaded   int64 `json:"uploaded"`

	Peers      int   `json:"peers"`
	UnchokedUs int   `json:"unchoked_us"`
	Interested int   `json:"interested"`
	Requests   int   `json:"requests"`
	KnownPeers int   `json:"known_peers"`
	Banned     int   `json:"banned"`
	Buffered   int64 `json:"buffered"`

	Dials           int64 `json:"dials"`
	DialFailures    int64 `json:"dial_failures"`
	Accepted        int64 `json:"accepted"`
	HashFailures    int64 `json:"hash_failures"`
	RequestTimeouts int64 `json:"request_timeouts"`
}

// Errors from Manager methods.
var (
	ErrExists   = errors.New("torrent already added")
	ErrNotFound = errors.New("no such torrent")
	ErrNoSource = errors.New("source needs a magnet link or a .torrent file")
)

// Manager runs many torrents.
type Manager struct {
	cfg     Config
	log     *slog.Logger
	limiter *swarm.Limiter
	budget  *torrent.MemoryBudget

	ctx    context.Context // parent of every torrent's context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	torrents map[id.Hash]*entry
}

type entry struct {
	infoHash id.Hash
	src      Source
	addedAt  time.Time
	swarm    *swarm.Swarm
	cancel   context.CancelFunc
	done     chan struct{}
	log      *slog.Logger  // fixed at Add; carries the infohash
	complete chan struct{} // closed when every piece is verified

	mu               sync.Mutex
	state            State
	seedLimitReached bool
	err              error
	meta             *metainfo.Metainfo
	session          *torrent.Session
}

// New returns a Manager. Torrents can be added before Run is called;
// they start right away.
func New(cfg Config) *Manager {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.PeerID == (id.Hash{}) {
		cfg.PeerID = id.NewPeerID()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		cfg:      cfg,
		log:      cfg.Logger,
		budget:   torrent.NewMemoryBudget(cfg.MaxMemory),
		ctx:      ctx,
		cancel:   cancel,
		torrents: map[id.Hash]*entry{},
	}
	if cfg.MaxPeers > 0 {
		m.limiter = swarm.NewLimiter(cfg.MaxPeers)
	}
	return m
}

// Run accepts peer connections on the shared listener until ctx is
// cancelled, then stops every torrent.
func (m *Manager) Run(ctx context.Context) error {
	defer m.shutdown()

	if ln := m.cfg.Listener; ln != nil {
		stop := context.AfterFunc(ctx, func() { ln.Close() })
		defer stop()
		go m.acceptLoop(ctx, ln)
	}
	<-ctx.Done()
	return ctx.Err()
}

func (m *Manager) shutdown() {
	m.cancel()
	m.wg.Wait()
}

func (m *Manager) acceptLoop(ctx context.Context, ln net.Listener) {
	for {
		nc, err := ln.Accept()
		if err != nil {
			if ctx.Err() == nil {
				m.log.Warn("listener accept failed", "err", err)
			}
			return
		}
		go m.route(nc)
	}
}

// route completes an incoming handshake and hands the connection to
// the session for the torrent the peer asked for.
func (m *Manager) route(nc net.Conn) {
	var sess *torrent.Session
	c, err := peerwire.AcceptRouted(nc, func(h peerwire.Handshake) (peerwire.Handshake, bool) {
		sess = m.session(h.InfoHash)
		if sess == nil {
			return peerwire.Handshake{}, false
		}
		return sess.Handshake(), true
	}, m.cfg.Session.Conn)
	if err != nil {
		m.log.Debug("peer accept failed", "addr", nc.RemoteAddr(), "err", err)
		return
	}
	sess.AddConn(c)
}

// session returns the running session for infoHash, or nil.
func (m *Manager) session(infoHash id.Hash) *torrent.Session {
	m.mu.Lock()
	e := m.torrents[infoHash]
	m.mu.Unlock()
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.session
}

// Add starts a torrent and returns its infohash. With a Store, the
// torrent is saved so Restore brings it back after a restart.
func (m *Manager) Add(src Source) (id.Hash, error) {
	return m.add(src, time.Now(), true)
}

// Restore re-adds every torrent in the Store, using cached info dicts
// so magnet links don't fetch their metadata again. It returns how
// many were restored.
func (m *Manager) Restore() (int, error) {
	if m.cfg.Store == nil {
		return 0, nil
	}
	records, err := m.cfg.Store.Records()
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, r := range records {
		src, err := m.sourceFromRecord(r)
		if err == nil {
			_, err = m.add(src, r.AddedAt, false)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("restore %s: %w", r.InfoHash, err))
			continue
		}
		n++
	}
	m.log.Info("torrents restored", "count", n, "failed", len(errs))
	return n, errors.Join(errs...)
}

// sourceFromRecord rebuilds a Source, swapping a magnet link for its
// cached info dict when there is one.
func (m *Manager) sourceFromRecord(r Record) (Source, error) {
	src := Source{Magnet: r.Magnet, Torrent: r.Torrent, Dir: r.Dir, Trackers: r.Trackers, finished: r.Finished}
	for _, p := range r.Peers {
		a, err := netip.ParseAddrPort(p)
		if err != nil {
			return src, err
		}
		src.Peers = append(src.Peers, a)
	}
	if r.Magnet == "" {
		return src, nil
	}
	mag, err := metainfo.ParseMagnet(r.Magnet)
	if err != nil {
		return src, err
	}
	raw, ok, err := m.cfg.Store.LoadInfo(mag.InfoHash)
	if err != nil || !ok {
		return src, err
	}
	// Keep the magnet's trackers and name, but skip the metadata
	// fetch.
	src.restoredInfo = raw
	return src, nil
}

func (m *Manager) add(src Source, addedAt time.Time, persist bool) (id.Hash, error) {
	var meta *metainfo.Metainfo
	var mag *metainfo.Magnet
	var infoHash id.Hash
	switch {
	case src.Magnet != "":
		var err error
		if mag, err = metainfo.ParseMagnet(src.Magnet); err != nil {
			return id.Hash{}, err
		}
		infoHash = mag.InfoHash
		if src.restoredInfo != "" {
			if meta, err = metainfo.FromInfoDict(src.restoredInfo); err != nil {
				return id.Hash{}, fmt.Errorf("cached info dict: %w", err)
			}
			if meta.InfoHash() != infoHash {
				return id.Hash{}, errors.New("cached info dict does not match the infohash")
			}
		}
	case len(src.Torrent) > 0:
		var err error
		if meta, err = metainfo.New(string(src.Torrent)); err != nil {
			return id.Hash{}, err
		}
		infoHash = meta.InfoHash()
	default:
		return id.Hash{}, ErrNoSource
	}
	if src.Dir == "" {
		return id.Hash{}, errors.New("source needs a download dir")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		return id.Hash{}, errors.New("manager has shut down")
	}
	if _, ok := m.torrents[infoHash]; ok {
		return infoHash, ErrExists
	}

	if persist && m.cfg.Store != nil {
		r := Record{
			InfoHash: infoHash.Hex(),
			Magnet:   src.Magnet,
			Torrent:  src.Torrent,
			Dir:      src.Dir,
			Trackers: src.Trackers,
			AddedAt:  addedAt,
		}
		for _, p := range src.Peers {
			r.Peers = append(r.Peers, p.String())
		}
		if err := m.cfg.Store.Save(r); err != nil {
			return id.Hash{}, fmt.Errorf("save torrent: %w", err)
		}
	}

	ctx, cancel := context.WithCancel(m.ctx)
	e := &entry{
		infoHash: infoHash,
		src:      src,
		addedAt:  addedAt,
		swarm:    m.newSwarm(),
		cancel:   cancel,
		done:     make(chan struct{}),
		complete: make(chan struct{}),
		log:      m.log.With("infohash", infoHash.Hex()),
		state:    StateMetadata,
		meta:     meta,
	}
	if meta != nil {
		e.state = StateVerifying
	}
	m.torrents[infoHash] = e

	trackers := slices.Clone(src.Trackers)
	if m.cfg.UseTrackers {
		if mag != nil {
			trackers = append(trackers, mag.Trackers...)
		} else if meta.Announce() != "" {
			trackers = append(trackers, meta.Announce())
		}
	}

	if src.finished {
		e.state = StateFinished
		cancel()
		close(e.done)
		e.log.Info("torrent restored as finished", "dir", src.Dir)
		return infoHash, nil
	}

	e.log.Info("torrent added", "dir", src.Dir, "magnet", mag != nil, "trackers", len(trackers), "direct_peers", len(src.Peers))
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(e.done)
		m.runTorrent(ctx, e, trackers)
	}()
	return infoHash, nil
}

func (m *Manager) newSwarm() *swarm.Swarm {
	cfg := m.cfg.Swarm
	cfg.Limiter = m.limiter
	return swarm.New(cfg)
}

// runTorrent starts a torrent's peer sources, fetches its metadata if
// needed, and runs its session until ctx is cancelled.
func (m *Manager) runTorrent(ctx context.Context, e *entry, trackers []string) {
	// A span for the torrent's life in the daemon; the metadata fetch,
	// session and tracker announces are its children.
	ctx, span := otel.Tracer("github.com/cnnrznn/torrent-ai/pkg/daemon").Start(ctx, "daemon.torrent",
		trace.WithAttributes(attribute.String("torrent.infohash", e.infoHash.Hex()), attribute.String("torrent.dir", e.src.Dir)))
	defer func() {
		st := e.status()
		span.SetAttributes(attribute.String("torrent.name", st.Name), attribute.String("torrent.state", string(st.State)))
		if st.State == StateFailed {
			span.SetStatus(codes.Error, st.Error)
		}
		span.End()
	}()

	log := e.log
	e.swarm.Add(e.src.Peers...)
	if node := m.cfg.DHT; node != nil {
		go peersource.DHT(ctx, log.With("component", "dht"), node, e.swarm, e.infoHash, m.cfg.Port, m.cfg.DHTSource)
	}

	// Trackers are announced to for the torrent's whole life, with its
	// progress as it changes.
	base := tracker.Request{
		InfoHash: e.infoHash,
		PeerID:   m.cfg.PeerID,
		Port:     m.cfg.Port,
		NumWant:  m.cfg.NumWant,
	}
	for _, tr := range trackers {
		go peersource.TrackerLoop(ctx, log.With("component", "tracker"), e.swarm, tr, m.cfg.TrackerOptions, base, e.progress, e.complete, m.cfg.TrackerLoop)
	}

	e.mu.Lock()
	meta := e.meta
	e.mu.Unlock()

	if meta == nil {
		// The size is unknown until we have the metadata; any non-zero
		// left marks us as a leecher.
		metaCfg := m.cfg.Metadata
		metaCfg.Logger = log.With("component", "metadata")
		var err error
		meta, err = torrent.FetchMetadata(ctx, e.swarm, e.infoHash, m.cfg.PeerID, metaCfg)
		if err != nil {
			e.finish(ctx, log, err)
			return
		}
		e.mu.Lock()
		e.meta = meta
		e.mu.Unlock()
		if st := m.cfg.Store; st != nil {
			if err := st.SaveInfo(e.infoHash, meta.RawInfo()); err != nil {
				log.Warn("caching info dict failed", "err", err)
			}
		}
	}
	log = log.With("name", meta.Name())

	sessCfg := m.cfg.Session
	sessCfg.Dir = e.src.Dir
	sessCfg.PeerID = m.cfg.PeerID
	sessCfg.Listener = nil
	sessCfg.Logger = log.With("component", "session")
	sessCfg.Memory = m.budget
	sessCfg.MaxMemory = 0
	if node := m.cfg.DHT; node != nil {
		sessCfg.DHTPort = int(node.Addr().Port())
		sessCfg.OnPort = node.AddNode
	}
	e.mu.Lock()
	e.state = StateVerifying
	e.mu.Unlock()
	sess, err := torrent.New(meta, e.swarm, sessCfg)
	if err != nil {
		e.finish(ctx, log, err)
		return
	}

	e.mu.Lock()
	e.session = sess
	e.state = StateDownloading
	e.mu.Unlock()
	go func() {
		select {
		case <-sess.Complete():
			e.mu.Lock()
			e.state = StateSeeding
			e.mu.Unlock()
			close(e.complete)
			log.Info("torrent complete, seeding")
			m.enforceSeedLimits(ctx, e, sess, log)
		case <-ctx.Done():
		}
	}()

	err = sess.Run(ctx)
	e.finish(ctx, log, err)
}

// enforceSeedLimits stops a complete torrent once it reaches the seed
// ratio or seed time, marks it finished and saves that, so it isn't
// seeded again after a restart. It returns when ctx ends.
func (m *Manager) enforceSeedLimits(ctx context.Context, e *entry, sess *torrent.Session, log *slog.Logger) {
	if m.cfg.SeedRatio <= 0 && m.cfg.SeedTime <= 0 {
		return
	}
	completedAt := time.Now()
	check := min(10*time.Second, max(m.cfg.SeedTime/4, 10*time.Millisecond))
	ticker := time.NewTicker(check)
	defer ticker.Stop()

	size := float64(e.meta.Length())
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		st := sess.Stats()
		ratio := float64(st.Uploaded) / max(size, 1)
		var reason string
		switch {
		case m.cfg.SeedRatio > 0 && ratio >= m.cfg.SeedRatio:
			reason = "ratio"
		case m.cfg.SeedTime > 0 && time.Since(completedAt) >= m.cfg.SeedTime:
			reason = "time"
		default:
			continue
		}

		log.Info("seeding limit reached", "reason", reason, "ratio", ratio, "seeded_for", time.Since(completedAt).Round(time.Second))
		e.mu.Lock()
		e.seedLimitReached = true
		e.mu.Unlock()
		if st := m.cfg.Store; st != nil {
			if r, ok := m.record(e); ok {
				r.Finished = true
				if err := st.Save(r); err != nil {
					log.Warn("saving finished state failed", "err", err)
				}
			}
		}
		e.cancel()
		return
	}
}

// record rebuilds a torrent's Store record.
func (m *Manager) record(e *entry) (Record, bool) {
	records, err := m.cfg.Store.Records()
	if err != nil {
		return Record{}, false
	}
	for _, r := range records {
		if r.InfoHash == e.infoHash.Hex() {
			return r, true
		}
	}
	return Record{}, false
}

// progress reports the torrent to its trackers. Before the metadata is
// known the size is too, so it reports one byte left, marking us as a
// leecher.
func (e *entry) progress() peersource.Progress {
	e.mu.Lock()
	sess := e.session
	e.mu.Unlock()
	if sess == nil {
		return peersource.Progress{Left: 1}
	}
	st := sess.Stats()
	return peersource.Progress{Uploaded: int(st.Uploaded), Downloaded: int(st.Downloaded), Left: int(st.Left)}
}

// finish records why a torrent stopped.
func (e *entry) finish(ctx context.Context, log *slog.Logger, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.seedLimitReached {
		e.state = StateFinished
		return
	}
	if ctx.Err() != nil {
		e.state = StateStopped
		return
	}
	e.state = StateFailed
	e.err = err
	log.Error("torrent failed", "err", err)
}

// Remove stops a torrent and forgets it. With deleteFiles, its
// downloaded files are deleted too.
func (m *Manager) Remove(infoHash id.Hash, deleteFiles bool) error {
	m.mu.Lock()
	e, ok := m.torrents[infoHash]
	if ok {
		delete(m.torrents, infoHash)
	}
	m.mu.Unlock()
	if !ok {
		return ErrNotFound
	}

	e.cancel()
	<-e.done
	e.log.Info("torrent removed", "delete_files", deleteFiles)
	if st := m.cfg.Store; st != nil {
		if err := st.Delete(infoHash); err != nil {
			return err
		}
	}

	if !deleteFiles {
		return nil
	}
	e.mu.Lock()
	meta := e.meta
	e.mu.Unlock()
	if meta == nil {
		return nil
	}
	// The metainfo validated its name as a single path component, so
	// this stays inside Dir.
	return os.RemoveAll(filepath.Join(e.src.Dir, meta.Name()))
}

// Status returns one torrent's status.
func (m *Manager) Status(infoHash id.Hash) (Status, error) {
	m.mu.Lock()
	e, ok := m.torrents[infoHash]
	m.mu.Unlock()
	if !ok {
		return Status{}, ErrNotFound
	}
	return e.status(), nil
}

// List returns every torrent's status, oldest first.
func (m *Manager) List() []Status {
	m.mu.Lock()
	entries := make([]*entry, 0, len(m.torrents))
	for _, e := range m.torrents {
		entries = append(entries, e)
	}
	m.mu.Unlock()

	slices.SortFunc(entries, func(a, b *entry) int { return a.addedAt.Compare(b.addedAt) })
	out := make([]Status, len(entries))
	for i, e := range entries {
		out[i] = e.status()
	}
	return out
}

func (e *entry) status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()

	st := Status{
		InfoHash:   e.infoHash.Hex(),
		State:      e.state,
		Dir:        e.src.Dir,
		AddedAt:    e.addedAt,
		KnownPeers: e.swarm.Len(),
		Banned:     e.swarm.Banned(),
	}
	if e.err != nil {
		st.Error = e.err.Error()
	}
	if e.meta != nil {
		st.Name = e.meta.Name()
		st.Size = int64(e.meta.Length())
		st.Pieces = e.meta.NumPieces()
	}
	if e.session != nil {
		s := e.session.Stats()
		st.Have, st.Left = s.Have, s.Left
		st.Downloaded, st.Uploaded = s.Downloaded, s.Uploaded
		st.Peers, st.UnchokedUs, st.Buffered = s.Peers, s.UnchokedUs, s.Buffered
		st.Interested, st.Requests = s.Interested, s.Requests
		st.Dials, st.DialFailures, st.Accepted = s.Dials, s.DialFailures, s.Accepted
		st.HashFailures, st.RequestTimeouts = s.HashFailures, s.RequestTimeouts
	}
	return st
}

// ParseInfoHash parses a 40-character hex infohash, as the API and CLI
// take them.
func ParseInfoHash(s string) (id.Hash, error) {
	h, err := id.FromHex(strings.ToLower(s))
	if err != nil {
		return id.Hash{}, fmt.Errorf("invalid infohash %q: %w", s, err)
	}
	return h, nil
}
