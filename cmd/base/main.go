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
	"strings"
	"time"

	"github.com/cnnrznn/torrent-ai/pkg/dht"
	"github.com/cnnrznn/torrent-ai/pkg/id"
	"github.com/cnnrznn/torrent-ai/pkg/metainfo"
	"github.com/cnnrznn/torrent-ai/pkg/swarm"
	"github.com/cnnrznn/torrent-ai/pkg/torrent"
	"github.com/cnnrznn/torrent-ai/pkg/tracker"
)

// dhtInterval is how often the DHT lookup for the torrent repeats.
const dhtInterval = 5 * time.Minute

type config struct {
	dir     string
	port    int
	seed    bool
	dht     bool
	timeout time.Duration
}

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
	var cfg config
	flag.StringVar(&cfg.dir, "o", ".", "directory to download into")
	flag.IntVar(&cfg.port, "port", 6881, "TCP port for peers, and UDP port for the DHT (0 = pick a free one)")
	flag.BoolVar(&cfg.seed, "seed", false, "keep seeding after the download completes")
	flag.BoolVar(&cfg.dht, "dht", true, "find peers through the DHT")
	flag.DurationVar(&cfg.timeout, "timeout", 0, "give up after this long, exiting with code 3 (0 = no limit)")
	verbose := flag.Bool("v", false, "debug logging for every component (same as -debug=all)")
	debug := flag.String("debug", "", "comma-separated components to debug-log: all,"+strings.Join(components, ","))
	logFormat := flag.String("log-format", "text", "log format: text or json")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] <file.torrent | magnet:?...>\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "exit codes: 0 complete, 1 error, 2 usage, 3 timeout, 130 interrupted\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(exitUsage)
	}

	debugComponents := strings.Split(*debug, ",")
	if *verbose {
		debugComponents = append(debugComponents, "all")
	}
	log, err := newLogger(os.Stderr, *logFormat, debugComponents)
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

	mainLog.Info("starting",
		"target", flag.Arg(0),
		"dir", cfg.dir,
		"port", cfg.port,
		"dht", cfg.dht,
		"seed", cfg.seed,
		"timeout", cfg.timeout,
	)

	start := time.Now()
	err = run(ctx, log, flag.Arg(0), cfg)
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
	sw := swarm.New(swarm.Config{MaxPeers: 50})

	// A .torrent gives us the metainfo up front; a magnet gives only
	// the infohash and maybe some trackers.
	var m *metainfo.Metainfo
	var mag *metainfo.Magnet
	var infoHash id.Hash
	var err error
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

	// Listen first so -port 0 resolves to a real port, which the DHT
	// then shares. Run closes the listener; this covers early returns.
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.port))
	if err != nil {
		return err
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	var node *dht.Server
	if cfg.dht {
		node, err = dht.New(dht.Config{Addr: fmt.Sprintf(":%d", port), Logger: log.With("component", compDHT)})
		if err != nil {
			return fmt.Errorf("dht: %w", err)
		}
		go node.Run(ctx)
		go dhtPeers(ctx, log.With("component", compDHT), node, sw, infoHash, port)
	}
	mainLog.Info("listening", "port", port, "dht", node != nil)

	if mag != nil {
		mainLog.Info("fetching metadata", "name", mag.Name, "infohash", infoHash.Hex(), "trackers", len(mag.Trackers))
		for _, tr := range mag.Trackers {
			go announce(ctx, trackerLog, sw, tr, tracker.Request{
				InfoHash: infoHash,
				PeerID:   peerID,
				Port:     port,
				// The size is unknown until we have the metadata; any
				// non-zero value marks us as a leecher.
				Left:  1,
				Event: tracker.EventStarted,
			})
		}
		if m, err = torrent.FetchMetadata(ctx, sw, infoHash, peerID, log.With("component", compMetadata)); err != nil {
			return err
		}
	}
	mainLog.Info("loaded torrent", "name", m.Name(), "size", m.Length(), "pieces", m.NumPieces(), "infohash", infoHash.Hex())

	sessCfg := torrent.Config{
		Dir:      cfg.dir,
		PeerID:   peerID,
		Listener: ln,
		Logger:   log.With("component", compSession),
	}
	if node != nil {
		sessCfg.DHTPort = port
		sessCfg.OnPort = node.AddNode
	}
	s, err := torrent.New(m, sw, sessCfg)
	if err != nil {
		return err
	}

	if m.Announce() != "" {
		go announce(ctx, trackerLog, sw, m.Announce(), tracker.Request{
			InfoHash: infoHash,
			PeerID:   peerID,
			Port:     port,
			Left:     int(s.Stats().Left),
			Event:    tracker.EventStarted,
		})
	}

	errc := make(chan error, 1)
	go func() { errc <- s.Run(ctx) }()

	ticker := time.NewTicker(statusInterval)
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
			logStatus(mainLog, phase, st, last, sw, node)
			last = st
		}
	}
}

// statusInterval is how often the status line is logged.
const statusInterval = 5 * time.Second

// logStatus writes the periodic one-line summary of the session.
// Rates are bytes per second since the previous status line.
func logStatus(log *slog.Logger, phase string, st, last torrent.Stats, sw *swarm.Swarm, node *dht.Server) {
	secs := statusInterval.Seconds()
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

func announce(ctx context.Context, log *slog.Logger, sw *swarm.Swarm, url string, req tracker.Request) {
	t, err := tracker.New(url, tracker.WithLogger(log))
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
// line every minute so DHT health is visible without debug logs.
func dhtPeers(ctx context.Context, log *slog.Logger, node *dht.Server, sw *swarm.Swarm, infoHash id.Hash, port int) {
	go func() {
		ticker := time.NewTicker(time.Minute)
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

	for {
		if node.Nodes() == 0 {
			st, err := node.Bootstrap(ctx, dht.DefaultRouters)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Warn("dht bootstrap failed", "err", err, "lookup", st)
			} else {
				log.Info("dht bootstrapped", "nodes", node.Nodes(), "lookup", st)
			}
		}

		st, err := node.Announce(ctx, infoHash, port, func(peers []netip.AddrPort) {
			sw.Add(peers...)
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Warn("dht lookup failed", "err", err, "lookup", st)
		} else {
			log.Info("dht lookup done", "infohash", infoHash.Hex(), "nodes", node.Nodes(), "lookup", st)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(dhtInterval):
		}
	}
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
