package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/cnnrznn/torrent-ai/pkg/daemon"
	"github.com/cnnrznn/torrent-ai/pkg/dht"
	"github.com/cnnrznn/torrent-ai/pkg/id"
	"github.com/cnnrznn/torrent-ai/pkg/peersource"
	"github.com/cnnrznn/torrent-ai/pkg/tracker"
)

// daemonConfig holds the flags for the daemon and its clients.
type daemonConfig struct {
	stateDir  string
	apiSocket string // empty means <state-dir>/api.sock; "off" disables it
	apiAddr   string
	apiToken  string
}

// defaultStateDir is $XDG_STATE_HOME/torrentai, or
// ~/.local/state/torrentai.
func defaultStateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "torrentai")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "state", "torrentai")
	}
	return "torrentai-state"
}

func (c daemonConfig) socketPath() string {
	switch c.apiSocket {
	case "":
		return filepath.Join(c.stateDir, "api.sock")
	case "off":
		return ""
	}
	return c.apiSocket
}

// registerAPIFlags adds the flags both the daemon and its clients use
// to find the API.
func registerAPIFlags(r *registry, c *daemonConfig) {
	r.str(&c.stateDir, "state-dir", "directory for the daemon's state: torrents, cached info dicts, DHT nodes, and the API socket")
	r.str(&c.apiSocket, "api-socket", "unix socket for the API; empty means <state-dir>/api.sock, \"off\" disables it")
	r.str(&c.apiAddr, "api-addr", "also serve the API on this TCP address, e.g. 127.0.0.1:8737 (clients: connect here instead of the socket)")
	r.str(&c.apiToken, "api-token", "bearer token required on the TCP API; never logged")
	r.secret("api-token")
}

// serveCommand runs the daemon: torrentai serve.
var serveCommand = command{
	name: "serve",
	register: func(r *registry, cfg *config) {
		r.group("Daemon")
		registerAPIFlags(r, &cfg.daemon)
	},
	run: func(ctx context.Context, log *slog.Logger, cfg config, _ []string) error {
		return serve(ctx, log, cfg)
	},
}

// serve runs the daemon until ctx is cancelled.
func serve(ctx context.Context, log *slog.Logger, cfg config) error {
	mainLog := log.With("component", compMain)

	store, err := daemon.OpenStore(cfg.daemon.stateDir)
	if err != nil {
		return fmt.Errorf("state dir: %w", err)
	}
	downloadDir, err := filepath.Abs(cfg.dir)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.port))
	if err != nil {
		return err
	}
	port := ln.Addr().(*net.TCPAddr).Port

	var node *dht.Server
	if cfg.useDHT {
		if node, err = startDaemonDHT(ctx, log.With("component", compDHT), cfg, store, port); err != nil {
			ln.Close()
			return err
		}
		// Save the routing table once the torrents have stopped.
		defer saveDHT(log.With("component", compDHT), store, node)
	}

	m := daemon.New(daemon.Config{
		PeerID:      id.NewPeerID(),
		Listener:    ln,
		Port:        port,
		Logger:      log.With("component", compSession),
		DHT:         node,
		DHTSource:   cfg.peerSourceDHT(),
		UseTrackers: cfg.useTrackers,
		TrackerOptions: []tracker.Option{
			tracker.WithLogger(log.With("component", compTracker)),
			tracker.WithHTTPTimeout(cfg.trackerTimeout),
			tracker.WithUDPRetries(cfg.udpTimeout, cfg.udpRetries),
			tracker.WithUDPConnectionIDLifetime(cfg.udpConnTTL),
		},
		NumWant: cfg.numWant,
		TrackerLoop: peersource.TrackerLoopConfig{
			MinInterval: cfg.trackerMinWait,
		},
		MaxPeers:  cfg.swarm.MaxPeers,
		MaxMemory: cfg.session.MaxMemory,
		Store:     store,
		SeedRatio: cfg.seedRatio,
		SeedTime:  cfg.seedTime,
		Swarm:     cfg.swarm,
		Session:   cfg.session,
		Metadata:  cfg.metadata,
	})
	if err := registerDaemonMetrics(m, node, !cfg.aggregateMetrics); err != nil {
		ln.Close()
		return fmt.Errorf("metrics: %w", err)
	}
	mainLog.Info("daemon listening", "peer_port", port, "dht", node != nil, "state_dir", cfg.daemon.stateDir, "download_dir", downloadDir)

	if _, err := m.Restore(); err != nil {
		mainLog.Warn("some torrents could not be restored", "err", err)
	}

	api := daemon.NewAPI(m, daemon.APIConfig{
		Token:      cfg.daemon.apiToken,
		DefaultDir: downloadDir,
		Logger:     log.With("component", compMain),
		Ready: func() (bool, string) {
			if node != nil && node.Nodes() < cfg.dht.K {
				return false, fmt.Sprintf("dht has %d nodes, want %d", node.Nodes(), cfg.dht.K)
			}
			return true, ""
		},
	})
	stopAPI, err := serveAPI(ctx, mainLog, api, cfg.daemon)
	if err != nil {
		ln.Close()
		return err
	}
	defer stopAPI()

	go logDaemonStatusEvery(ctx, mainLog, m, node, cfg.statusInterval)

	return m.Run(ctx)
}

// startDaemonDHT starts the DHT node, keeping the node ID and routing
// table saved by the last run so it needn't bootstrap from scratch, and
// saves them periodically.
func startDaemonDHT(ctx context.Context, log *slog.Logger, cfg config, store *daemon.Store, port int) (*dht.Server, error) {
	dhtCfg := cfg.dht
	dhtCfg.Addr = fmt.Sprintf(":%d", port)
	dhtCfg.Logger = log

	saved, ok, err := store.LoadDHT()
	if err != nil {
		log.Warn("saved dht state unreadable, starting fresh", "err", err)
	}
	if ok {
		if h, err := id.FromHex(saved.ID); err == nil {
			dhtCfg.ID = h
		}
	}

	node, err := dht.New(dhtCfg)
	if err != nil {
		return nil, fmt.Errorf("dht: %w", err)
	}
	if ok {
		added := node.Seed(saved.Nodes)
		log.Info("dht routing table restored", "nodes", added, "saved", len(saved.Nodes), "age", time.Since(saved.SavedAt).Round(time.Second))
	}

	go labeled(ctx, compDHT, func(ctx context.Context) { node.Run(ctx) })
	go labeled(ctx, compDHT, func(ctx context.Context) { logDHTStatsEvery(ctx, log, node, cfg.dhtStatsInterval) })
	// Join the DHT now, not when the first torrent is added: readiness
	// depends on it.
	go labeled(ctx, compDHT, func(ctx context.Context) { peersource.KeepBootstrapped(ctx, log, node, cfg.peerSourceDHT()) })
	go labeled(ctx, compDHT, func(ctx context.Context) {
		ticker := time.NewTicker(cfg.dhtSaveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				saveDHT(log, store, node)
			}
		}
	})
	return node, nil
}

func saveDHT(log *slog.Logger, store *daemon.Store, node *dht.Server) {
	nodes := node.GoodNodes()
	if len(nodes) == 0 {
		return // keep the last good save rather than an empty one
	}
	st := daemon.DHTState{ID: node.ID().Hex(), Nodes: nodes, SavedAt: time.Now().UTC()}
	if err := store.SaveDHT(st); err != nil {
		log.Warn("saving dht state failed", "err", err)
		return
	}
	log.Debug("dht state saved", "nodes", len(nodes))
}

// serveAPI starts the API on the unix socket and, if set, the TCP
// address. The returned function stops both.
func serveAPI(ctx context.Context, log *slog.Logger, h http.Handler, c daemonConfig) (func(), error) {
	var servers []*http.Server
	var cleanup []func()
	stop := func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		for _, s := range servers {
			s.Shutdown(shutdownCtx)
		}
		for _, f := range cleanup {
			f()
		}
	}
	start := func(ln net.Listener) {
		srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, srv)
		go srv.Serve(ln)
	}

	if sock := c.socketPath(); sock != "" {
		// A socket left by a crashed daemon would block the listen.
		if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
			return stop, err
		}
		ln, err := net.Listen("unix", sock)
		if err != nil {
			return stop, fmt.Errorf("api socket: %w", err)
		}
		// Only this user may use the socket.
		if err := os.Chmod(sock, 0o600); err != nil {
			ln.Close()
			return stop, err
		}
		cleanup = append(cleanup, func() { os.Remove(sock) })
		start(ln)
		log.Info("api listening", "socket", sock)
	}

	if c.apiAddr != "" {
		ln, err := net.Listen("tcp", c.apiAddr)
		if err != nil {
			stop()
			return func() {}, fmt.Errorf("api address: %w", err)
		}
		start(ln)
		addr := ln.Addr().(*net.TCPAddr)
		log.Info("api listening", "addr", addr.String(), "token", c.apiToken != "")
		if c.apiToken == "" && !addr.IP.IsLoopback() {
			log.Warn("api reachable without a token", "addr", addr.String(), "hint", "set -api-token, or listen on 127.0.0.1")
		}
	}
	return stop, nil
}

// logDaemonStatusEvery logs a summary of every torrent each interval.
func logDaemonStatusEvery(ctx context.Context, log *slog.Logger, m *daemon.Manager, node *dht.Server, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastDown, lastUp int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		states := map[daemon.State]int{}
		var down, up int64
		var peers int
		var buffered int64
		list := m.List()
		for _, st := range list {
			states[st.State]++
			down += st.Downloaded
			up += st.Uploaded
			peers += st.Peers
			buffered += st.Buffered
		}
		secs := interval.Seconds()
		attrs := []any{
			"torrents", len(list),
			"metadata", states[daemon.StateMetadata],
			"verifying", states[daemon.StateVerifying],
			"downloading", states[daemon.StateDownloading],
			"seeding", states[daemon.StateSeeding],
			"finished", states[daemon.StateFinished],
			"failed", states[daemon.StateFailed],
			"down_rate", int64(float64(down-lastDown) / secs),
			"up_rate", int64(float64(up-lastUp) / secs),
			"peers", peers,
			"buffered", buffered,
		}
		if node != nil {
			attrs = append(attrs, "dht_nodes", node.Nodes())
		}
		log.Info("daemon status", attrs...)
		lastDown, lastUp = down, up
	}
}
