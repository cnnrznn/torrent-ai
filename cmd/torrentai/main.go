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
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/cnnrznn/torrent-ai/pkg/dht"
	"github.com/cnnrznn/torrent-ai/pkg/id"
	"github.com/cnnrznn/torrent-ai/pkg/metainfo"
	"github.com/cnnrznn/torrent-ai/pkg/peersource"
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
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "serve":
			os.Exit(runCommand(serveCommand, args[1:]))
		case "add", "ls", "status", "rm":
			os.Exit(clientMain(args[0], args[1:]))
		}
	}
	os.Exit(runCommand(downloadCommand, args))
}

// command is one way of running torrentai with the full runtime:
// flags and environment, logging, telemetry, profiling and signals.
type command struct {
	name     string // shown in usage, after the program name
	usage    string // positional arguments
	args     int    // number of positional arguments required
	register func(*registry, *config)
	// rootSpan names a span covering the whole run; empty for none.
	rootSpan string
	run      func(ctx context.Context, log *slog.Logger, cfg config, args []string) error
}

// downloadCommand is the one-shot download: torrentai <magnet|file>.
var downloadCommand = command{
	usage:    "<file.torrent | magnet:?...>",
	args:     1,
	rootSpan: "torrentai.run",
	run: func(ctx context.Context, log *slog.Logger, cfg config, args []string) error {
		trace.SpanFromContext(ctx).SetAttributes(attribute.String("torrent.target", redactTarget(args[0])))
		return run(ctx, log, args[0], cfg)
	},
}

// runCommand runs cmd and returns the process exit code.
func runCommand(cmd command, args []string) int {
	cfg := defaultConfig()
	name := filepath.Base(os.Args[0])
	if cmd.name != "" {
		name += " " + cmd.name
	}
	r := newRegistry(name, cmd.usage)
	if cmd.name == "" {
		r.notes = fmt.Sprintf(`
Downloads one torrent and exits. To run many, use the daemon:
  %[1]s serve [flags]                     run the daemon
  %[1]s add [flags] <magnet|file>         add a torrent to it
  %[1]s ls | status <infohash> | rm <infohash>
Each takes -h.
`, filepath.Base(os.Args[0]))
	}
	registerFlags(r, &cfg)
	if cmd.register != nil {
		cmd.register(r, &cfg)
	}
	r.fs.SetOutput(os.Stderr)
	r.fs.Usage = func() { r.printUsage(os.Stderr) }

	if err := r.parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitComplete
		}
		fmt.Fprintf(os.Stderr, "%v\nrun with -h for help\n", err)
		return exitUsage
	}
	if r.fs.NArg() != cmd.args {
		r.printUsage(os.Stderr)
		return exitUsage
	}
	if err := cfg.validate(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\nrun with -h for help\n", err)
		return exitUsage
	}
	cfg.finish()

	ctx, stop := withStopSignals(context.Background())
	defer stop()

	tel, err := setupTelemetry(ctx, cfg.otel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "opentelemetry: %v\n", err)
		return exitError
	}

	debugComponents := strings.Split(cfg.debug, ",")
	if cfg.verbose {
		debugComponents = append(debugComponents, "all")
	}
	log, err := newLogger(os.Stderr, cfg.logFormat, debugComponents, tel.logHandler)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitUsage
	}
	mainLog := log.With("component", compMain)
	context.AfterFunc(ctx, func() {
		var sig *signalError
		if errors.As(context.Cause(ctx), &sig) {
			mainLog.Info("shutting down", "signal", sig.signalName(), "hint", "send it again to exit immediately")
		}
	})
	if cfg.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, cfg.timeout, errTimeout)
		defer cancel()
	}

	startAttrs := []any{"version", version()}
	if cmd.name != "" {
		startAttrs = append(startAttrs, "command", cmd.name)
	}
	if cmd.args > 0 {
		startAttrs = append(startAttrs, "target", r.fs.Arg(0))
	}
	mainLog.Info("starting", startAttrs...)
	mainLog.Info("config", r.values()...)
	if len(r.fromEnv) > 0 {
		mainLog.Info("config from environment", "flags", r.fromEnv)
	}
	for _, o := range r.specOverrides() {
		mainLog.Warn("spec default overridden", "flag", o.name, "value", o.value, "default", o.def, "spec", o.spec)
	}

	stopProfiling, err := cfg.profiling.start(ctx, mainLog)
	if err != nil {
		stopProfiling()
		mainLog.Error("exit", "result", "error", "err", err, "elapsed", time.Duration(0), "code", exitError)
		return exitError
	}

	uploader, err := cfg.profiling.pyroscope.uploader(log.With("component", compMain))
	if err != nil {
		stopProfiling()
		mainLog.Error("exit", "result", "error", "err", err, "elapsed", time.Duration(0), "code", exitError)
		return exitError
	}
	uploadCtx, stopUploads := context.WithCancel(ctx)
	uploadsDone := make(chan struct{})
	if uploader != nil {
		go func() { uploader.Run(uploadCtx); close(uploadsDone) }()
	} else {
		close(uploadsDone)
	}

	if cfg.otel.enabled {
		mainLog.Info("opentelemetry enabled",
			"traces", envOr("OTEL_TRACES_EXPORTER", "otlp"),
			"metrics", envOr("OTEL_METRICS_EXPORTER", "otlp"),
			"logs", envOr("OTEL_LOGS_EXPORTER", "otlp"),
			"endpoint", envOr("OTEL_EXPORTER_OTLP_ENDPOINT", "(default)"),
			"protocol", envOr("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf"),
			"service", os.Getenv("OTEL_SERVICE_NAME"),
		)
	}

	runCtx := ctx
	span := trace.SpanFromContext(ctx)
	if cmd.rootSpan != "" {
		runCtx, span = otel.Tracer(instrumentationName).Start(ctx, cmd.rootSpan)
	}
	start := time.Now()
	err = cmd.run(runCtx, log, cfg, r.fs.Args())
	elapsed := time.Since(start).Round(time.Millisecond)
	if err != nil && ctx.Err() == nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
	// The final profile upload, then profile files.
	stopUploads()
	<-uploadsDone
	stopProfiling()

	// Log the exit before flushing telemetry, so the exit line is
	// exported too.
	code := logExit(mainLog, ctx, err, elapsed)

	shutdownCtx, cancelShutdown := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	if err := tel.shutdown(shutdownCtx); err != nil {
		mainLog.Warn("opentelemetry shutdown failed", "err", err)
	}
	cancelShutdown()
	return code
}

// logExit writes the final exit line and returns the exit code.
func logExit(log *slog.Logger, ctx context.Context, err error, elapsed time.Duration) int {
	var sig *signalError
	switch {
	case err == nil:
		log.Info("exit", "result", "complete", "elapsed", elapsed, "code", exitComplete)
		return exitComplete
	case errors.Is(context.Cause(ctx), errTimeout):
		log.Error("exit", "result", "timeout", "elapsed", elapsed, "code", exitTimeout)
		return exitTimeout
	case errors.As(context.Cause(ctx), &sig):
		log.Info("exit", "result", "interrupted", "signal", sig.signalName(), "elapsed", elapsed, "code", sig.exitCode())
		return sig.exitCode()
	default:
		log.Error("exit", "result", "error", "err", err, "elapsed", elapsed, "code", exitError)
		return exitError
	}
}

func run(ctx context.Context, log *slog.Logger, arg string, cfg config) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	mainLog := log.With("component", compMain)
	trackerLog := log.With("component", compTracker)

	peerID := id.NewPeerID()
	sw := swarm.New(cfg.swarm)
	var session atomic.Pointer[torrent.Session]

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
		go labeled(ctx, compDHT, func(ctx context.Context) { node.Run(ctx) })
		go labeled(ctx, compDHT, func(ctx context.Context) { logDHTStatsEvery(ctx, dhtCfg.Logger, node, cfg.dhtStatsInterval) })
		go labeled(ctx, compDHT, func(ctx context.Context) {
			peersource.DHT(ctx, dhtCfg.Logger, node, sw, infoHash, port, cfg.peerSourceDHT())
		})
	}
	mainLog.Info("listening", "port", port, "dht", node != nil)
	if err := registerMetrics(&session, sw, node); err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("torrent.infohash", infoHash.Hex()))

	if mag != nil {
		mainLog.Info("fetching metadata", "name", mag.Name, "infohash", infoHash.Hex(), "trackers", len(trackers), "direct_peers", len(direct))
		for _, tr := range trackers {
			req := tracker.Request{
				InfoHash: infoHash,
				PeerID:   peerID,
				Port:     port,
				// The size is unknown until we have the metadata; any
				// non-zero value marks us as a leecher.
				Left:    1,
				Event:   tracker.EventStarted,
				NumWant: cfg.numWant,
			}
			go labeled(ctx, compTracker, func(ctx context.Context) { peersource.Tracker(ctx, trackerLog, sw, tr, trackerOpts, req) })
		}
		metaCfg := cfg.metadata
		metaCfg.Logger = log.With("component", compMetadata)
		labeled(ctx, compMetadata, func(ctx context.Context) {
			m, err = torrent.FetchMetadata(ctx, sw, infoHash, peerID, metaCfg)
		})
		if err != nil {
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
	session.Store(s)

	if mag == nil {
		for _, tr := range trackers {
			req := tracker.Request{
				InfoHash: infoHash,
				PeerID:   peerID,
				Port:     port,
				Left:     int(s.Stats().Left),
				Event:    tracker.EventStarted,
				NumWant:  cfg.numWant,
			}
			go labeled(ctx, compTracker, func(ctx context.Context) { peersource.Tracker(ctx, trackerLog, sw, tr, trackerOpts, req) })
		}
	}

	errc := make(chan error, 1)
	go labeled(ctx, compSession, func(ctx context.Context) { errc <- s.Run(ctx) })

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

// envOr returns the environment variable k, or def if it's unset.
func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// redactTarget keeps a .torrent path or a magnet link's infohash and
// name, dropping tracker URLs that may carry passkeys.
func redactTarget(target string) string {
	if !strings.HasPrefix(target, "magnet:") {
		return target
	}
	mag, err := metainfo.ParseMagnet(target)
	if err != nil {
		return "magnet:(invalid)"
	}
	return "magnet:?xt=urn:btih:" + mag.InfoHash.Hex()
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

// logDHTStatsEvery logs the DHT node's counters every interval until
// ctx is cancelled, so DHT health is visible without debug logs.
func logDHTStatsEvery(ctx context.Context, log *slog.Logger, node *dht.Server, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			logDHTStats(log, node.Stats())
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
