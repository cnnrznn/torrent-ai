package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/cnnrznn/torrent-ai/pkg/dht"
	"github.com/cnnrznn/torrent-ai/pkg/id"
	"github.com/cnnrznn/torrent-ai/pkg/metainfo"
	"github.com/cnnrznn/torrent-ai/pkg/swarm"
	"github.com/cnnrznn/torrent-ai/pkg/torrent"
	"github.com/cnnrznn/torrent-ai/pkg/tracker"
)

// Exit codes.
const (
	exitComplete    = 0
	exitError       = 1
	exitUsage       = 2
	exitTimeout     = 3
	exitInterrupted = 130
)

var errTimeout = errors.New("timeout")

func main() {
	cfg := defaultConfig()
	r := newRegistry(filepath.Base(os.Args[0]), "<file.torrent | magnet:?...>")
	registerFlags(r, &cfg)
	r.fs.SetOutput(os.Stderr)
	r.fs.Usage = func() { r.printUsage(os.Stderr) }

	if err := r.parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(exitComplete)
		}
		fmt.Fprintf(os.Stderr, "%v\nrun with -h for help\n", err)
		os.Exit(exitUsage)
	}
	if r.fs.NArg() != 1 {
		r.printUsage(os.Stderr)
		os.Exit(exitUsage)
	}
	if err := cfg.validate(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\nrun with -h for help\n", err)
		os.Exit(exitUsage)
	}
	cfg.finish()
	target := r.fs.Arg(0)

	debugComponents := strings.Split(cfg.debug, ",")
	if cfg.verbose {
		debugComponents = append(debugComponents, "all")
	}
	log, err := newLogger(os.Stderr, cfg.logFormat, debugComponents)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(exitUsage)
	}
	mainLog := log.With("component", compMain)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if cfg.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, cfg.timeout, errTimeout)
		defer cancel()
	}

	mainLog.Info("starting", "target", target)
	mainLog.Info("config", r.values()...)
	if len(r.fromEnv) > 0 {
		mainLog.Info("config from environment", "flags", r.fromEnv)
	}
	for _, o := range r.specOverrides() {
		mainLog.Warn("spec default overridden", "flag", o.name, "value", o.value, "default", o.def, "spec", o.spec)
	}

	start := time.Now()
	err = run(ctx, log, target, cfg)
	elapsed := time.Since(start).Round(time.Millisecond)

	switch {
	case err == nil:
		mainLog.Info("exit", "result", "complete", "elapsed", elapsed, "code", exitComplete)
		os.Exit(exitComplete)
	case errors.Is(context.Cause(ctx), errTimeout):
		mainLog.Error("exit", "result", "timeout", "elapsed", elapsed, "code", exitTimeout)
		os.Exit(exitTimeout)
	case ctx.Err() != nil:
		mainLog.Info("exit", "result", "interrupted", "elapsed", elapsed, "code", exitInterrupted)
		os.Exit(exitInterrupted)
	default:
		mainLog.Error("exit", "result", "error", "err", err, "elapsed", elapsed, "code", exitError)
		os.Exit(exitError)
	}
}

func run(ctx context.Context, log *slog.Logger, arg string, cfg config) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	mainLog := log.With("component", compMain)
	trackerLog := log.With("component", compTracker)

	peerID := id.NewPeerID()
	sw := swarm.New(cfg.swarm)

	direct, err := resolvePeers(cfg.peers)
	if err != nil {
		return err
	}
	sw.Add(direct...)

	// A .torrent gives us the metainfo up front; a magnet gives only
	// the infohash and maybe some trackers.
	var m *metainfo.Metainfo
	var mag *metainfo.Magnet
	var infoHash id.Hash
	if strings.HasPrefix(arg, "magnet:") {
		if mag, err = metainfo.ParseMagnet(arg); err != nil {
			return err
		}
		infoHash = mag.InfoHash
	} else {
		if m, err = fromFile(arg); err != nil {
			return err
		}
		infoHash = m.InfoHash()
	}

	// Trackers from the torrent or magnet link, unless disabled, plus
	// any given with -tracker.
	var trackers []string
	if cfg.useTrackers {
		if mag != nil {
			trackers = append(trackers, mag.Trackers...)
		} else if m.Announce() != "" {
			trackers = append(trackers, m.Announce())
		}
	}
	trackers = append(trackers, cfg.trackers...)
	trackerOpts := []tracker.Option{
		tracker.WithLogger(trackerLog),
		tracker.WithHTTPTimeout(cfg.trackerTimeout),
		tracker.WithUDPRetries(cfg.udpTimeout, cfg.udpRetries),
		tracker.WithUDPConnectionIDLifetime(cfg.udpConnTTL),
	}

	if !cfg.useDHT && len(trackers) == 0 && len(direct) == 0 {
		mainLog.Warn("no peer sources", "hint", "enable -dht or -trackers, or add -tracker or -peer")
	}

	// Listen first so -port 0 resolves to a real port, which the DHT
	// then shares. Run closes the listener; this covers early returns.
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.port))
	if err != nil {
		return err
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	var node *dht.Server
	if cfg.useDHT {
		dhtCfg := cfg.dht
		dhtCfg.Addr = fmt.Sprintf(":%d", port)
		dhtCfg.Logger = log.With("component", compDHT)
		node, err = dht.New(dhtCfg)
		if err != nil {
			return fmt.Errorf("dht: %w", err)
		}
		go node.Run(ctx)
		go dhtPeers(ctx, dhtCfg.Logger, node, sw, infoHash, port, cfg)
	}
	mainLog.Info("listening", "port", port, "dht", node != nil)

	if mag != nil {
		mainLog.Info("fetching metadata", "name", mag.Name, "infohash", infoHash.Hex(), "trackers", len(trackers), "direct_peers", len(direct))
		for _, tr := range trackers {
			go announce(ctx, trackerLog, sw, tr, trackerOpts, tracker.Request{
				InfoHash: infoHash,
				PeerID:   peerID,
				Port:     port,
				// The size is unknown until we have the metadata; any
				// non-zero value marks us as a leecher.
				Left:    1,
				Event:   tracker.EventStarted,
				NumWant: cfg.numWant,
			})
		}
		metaCfg := cfg.metadata
		metaCfg.Logger = log.With("component", compMetadata)
		if m, err = torrent.FetchMetadata(ctx, sw, infoHash, peerID, metaCfg); err != nil {
			return err
		}
	}
	mainLog.Info("loaded torrent", "name", m.Name(), "size", m.Length(), "pieces", m.NumPieces(), "infohash", infoHash.Hex())

	sessCfg := cfg.session
	sessCfg.PeerID = peerID
	sessCfg.Listener = ln
	sessCfg.Logger = log.With("component", compSession)
	if node != nil {
		sessCfg.DHTPort = port
		sessCfg.OnPort = node.AddNode
	}
	s, err := torrent.New(m, sw, sessCfg)
	if err != nil {
		return err
	}

	if mag == nil {
		for _, tr := range trackers {
			go announce(ctx, trackerLog, sw, tr, trackerOpts, tracker.Request{
				InfoHash: infoHash,
				PeerID:   peerID,
				Port:     port,
				Left:     int(s.Stats().Left),
				Event:    tracker.EventStarted,
				NumWant:  cfg.numWant,
			})
		}
	}

	errc := make(chan error, 1)
	go func() { errc <- s.Run(ctx) }()

	ticker := time.NewTicker(cfg.statusInterval)
	defer ticker.Stop()

	phase := "downloading"
	var last torrent.Stats
	complete := s.Complete()
	for {
		select {
		case err := <-errc:
			return err
		case <-complete:
			complete = nil
			if !cfg.seed {
				cancel()
				<-errc
				return nil
			}
			phase = "seeding"
			mainLog.Info("seeding", "hint", "interrupt to stop")
		case <-ticker.C:
			st := s.Stats()
			logStatus(mainLog, phase, st, last, cfg.statusInterval, sw, node)
			last = st
		}
	}
}

// logStatus writes the periodic one-line summary of the session.
// Rates are bytes per second since the previous status line.
func logStatus(log *slog.Logger, phase string, st, last torrent.Stats, interval time.Duration, sw *swarm.Swarm, node *dht.Server) {
	secs := interval.Seconds()
	attrs := []any{
		"phase", phase,
		"pieces", st.Have,
		"of", st.Pieces,
		"percent", fmt.Sprintf("%.1f", 100*float64(st.Have)/float64(st.Pieces)),
		"left", st.Left,
		"down", st.Downloaded,
		"down_rate", int64(float64(st.Downloaded-last.Downloaded) / secs),
		"up", st.Uploaded,
		"up_rate", int64(float64(st.Uploaded-last.Uploaded) / secs),
		"peers", st.Peers,
		"unchoked_us", st.UnchokedUs,
		"interested", st.Interested,
		"requests", st.Requests,
		"buffered", st.Buffered,
		"known", sw.Len(),
		"banned", sw.Banned(),
		"dials", st.Dials,
		"dial_failures", st.DialFailures,
		"accepted", st.Accepted,
		"hash_failures", st.HashFailures,
		"request_timeouts", st.RequestTimeouts,
	}
	if node != nil {
		attrs = append(attrs, "dht_nodes", node.Nodes())
	}
	log.Info("status", attrs...)
}

func fromFile(path string) (*metainfo.Metainfo, error) {
	bs, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return metainfo.New(string(bs))
}

// resolvePeers turns -peer host:port values into addresses.
func resolvePeers(hosts []string) ([]netip.AddrPort, error) {
	var addrs []netip.AddrPort
	for _, h := range hosts {
		a, err := net.ResolveTCPAddr("tcp", h)
		if err != nil {
			return nil, fmt.Errorf("-peer %s: %w", h, err)
		}
		addrs = append(addrs, a.AddrPort())
	}
	return addrs, nil
}

func announce(ctx context.Context, log *slog.Logger, sw *swarm.Swarm, url string, opts []tracker.Option, req tracker.Request) {
	t, err := tracker.New(url, opts...)
	if err != nil {
		log.Warn("tracker invalid", "url", tracker.Redact(url), "err", err)
		return
	}
	start := time.Now()
	res, err := t.Announce(ctx, req)
	if err != nil {
		if ctx.Err() == nil {
			log.Warn("tracker announce failed", "url", tracker.Redact(url), "err", err, "dur", time.Since(start).Round(time.Millisecond))
		}
		return
	}
	log.Info("tracker announce done", "url", tracker.Redact(url), "peers", len(res.Peers), "interval", res.Interval, "dur", time.Since(start).Round(time.Millisecond))
	sw.Add(res.Peers...)
}

// dhtPeers joins the DHT and repeatedly looks up and announces the
// torrent, feeding the peers it finds into the swarm. It logs a stats
// line periodically so DHT health is visible without debug logs.
func dhtPeers(ctx context.Context, log *slog.Logger, node *dht.Server, sw *swarm.Swarm, infoHash id.Hash, port int, cfg config) {
	go func() {
		ticker := time.NewTicker(cfg.dhtStatsInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				logDHTStats(log, node.Stats())
			}
		}
	}()

	retry := cfg.dhtRetryInterval
	for {
		// A table with fewer than k nodes can't support a useful
		// lookup, so keep bootstrapping until it fills.
		if node.Nodes() < cfg.dht.K {
			st, err := node.Bootstrap(ctx, cfg.dhtRouters)
			if ctx.Err() != nil {
				return
			}
			switch {
			case err != nil:
				log.Warn("dht bootstrap failed", "err", err, "nodes", node.Nodes(), "lookup", st)
			case node.Nodes() < cfg.dht.K:
				log.Warn("dht bootstrap weak", "nodes", node.Nodes(), "want", cfg.dht.K, "lookup", st)
			default:
				log.Info("dht bootstrapped", "nodes", node.Nodes(), "lookup", st)
			}
		}

		st, err := node.Announce(ctx, infoHash, port, func(peers []netip.AddrPort) {
			sw.Add(peers...)
		})
		if ctx.Err() != nil {
			return
		}

		var wait time.Duration
		wait, retry = nextLookup(node.Nodes() >= cfg.dht.K && err == nil && st.Peers > 0, retry, cfg)
		if err != nil {
			log.Warn("dht lookup failed", "err", err, "lookup", st, "next", wait)
		} else {
			log.Info("dht lookup done", "infohash", infoHash.Hex(), "nodes", node.Nodes(), "lookup", st, "next", wait)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// nextLookup decides how long to wait before the next DHT lookup. A
// healthy lookup waits the full -dht-interval. Otherwise (too few
// nodes, an error, or no peers) it retries after retry, doubling the
// retry each time up to the interval. It returns the wait and the
// retry to use next time.
func nextLookup(healthy bool, retry time.Duration, cfg config) (wait, nextRetry time.Duration) {
	if healthy {
		return cfg.dhtInterval, cfg.dhtRetryInterval
	}
	return retry, min(2*retry, cfg.dhtInterval)
}

func logDHTStats(log *slog.Logger, st dht.Stats) {
	log.Info("dht stats",
		"nodes", st.Nodes,
		"buckets", st.Buckets,
		"stored_peers", st.StoredPeers,
		"sent", st.QueriesSent,
		"replies", st.Replies,
		"error_replies", st.ErrorReplies,
		"timeouts", st.Timeouts,
		"received", st.QueriesReceived,
		"announced_to_us", st.Announced,
		"bad_packets", st.BadPackets,
	)
}
