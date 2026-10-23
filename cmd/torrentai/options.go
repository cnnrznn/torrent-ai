package main

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/cnnrznn/torrent-ai/pkg/dht"
	"github.com/cnnrznn/torrent-ai/pkg/peersource"
	"github.com/cnnrznn/torrent-ai/pkg/peerwire"
	"github.com/cnnrznn/torrent-ai/pkg/swarm"
	"github.com/cnnrznn/torrent-ai/pkg/torrent"
	"github.com/cnnrznn/torrent-ai/pkg/tracker"
)

// config is everything the command line controls. Package settings are
// bound directly to the flags, starting from each package's defaults.
type config struct {
	dir            string
	port           int
	seed           bool
	timeout        time.Duration
	statusInterval time.Duration

	logFormat string
	debug     string
	verbose   bool

	peers []string

	useTrackers    bool
	trackers       []string
	trackerTimeout time.Duration
	numWant        int
	trackerMinWait time.Duration
	udpTimeout     time.Duration
	udpRetries     int
	udpConnTTL     time.Duration

	useDHT           bool
	dhtRouters       []string
	dhtInterval      time.Duration
	dhtRetryInterval time.Duration
	dhtStatsInterval time.Duration
	dhtSaveInterval  time.Duration

	aggregateMetrics bool

	seedRatio float64
	seedTime  time.Duration

	profiling profiling
	otel      otelConfig
	daemon    daemonConfig

	swarm    swarm.Config
	session  torrent.Config
	metadata torrent.MetadataConfig
	dht      dht.Config
}

func defaultConfig() config {
	cfg := config{
		dir:            ".",
		port:           6881,
		statusInterval: 5 * time.Second,
		logFormat:      "text",

		useTrackers:    true,
		trackerTimeout: 10 * time.Second,
		trackerMinWait: time.Minute,
		udpTimeout:     15 * time.Second,
		udpRetries:     8,
		udpConnTTL:     time.Minute,

		useDHT:           true,
		dhtRouters:       append([]string(nil), dht.DefaultRouters...),
		dhtInterval:      5 * time.Minute,
		dhtRetryInterval: 5 * time.Second,
		dhtStatsInterval: time.Minute,
		dhtSaveInterval:  5 * time.Minute,

		daemon:    daemonConfig{stateDir: defaultStateDir()},
		profiling: profiling{pyroscope: pyroscopeConfig{app: "torrentai", interval: time.Minute}},
		swarm:     swarm.DefaultConfig(),
		session:   torrent.DefaultConfig(),
		metadata:  torrent.DefaultMetadataConfig(),
		dht:       dht.DefaultConfig(),
	}
	cfg.session.Conn = peerwire.DefaultOptions()
	cfg.session.Metadata = peerwire.DefaultMetadataOptions()
	return cfg
}

// registerFlags binds every setting in cfg to a flag.
func registerFlags(r *registry, cfg *config) {
	s := &cfg.session
	conn := &cfg.session.Conn
	meta := &cfg.session.Metadata

	r.group("General")
	r.str(&cfg.dir, "o", "directory to download into")
	r.integer(&cfg.port, "port", 0, "TCP port for peers and UDP port for the DHT; 0 picks a free one", "")
	r.boolean(&cfg.seed, "seed", "keep seeding after the download completes")
	r.float(&cfg.seedRatio, "seed-ratio", 0, "serve: stop seeding a torrent after uploading this many times its size; 0 means no limit", "")
	r.duration(&cfg.seedTime, "seed-time", 0, "serve: stop seeding a torrent this long after it completes; 0 means no limit", "")
	r.duration(&cfg.timeout, "timeout", 0, "give up after this long, exiting with code 3; 0 means no limit", "")
	r.duration(&cfg.statusInterval, "status-interval", time.Millisecond, "how often the status line is logged", "")

	r.group("Logging")
	r.str(&cfg.logFormat, "log-format", "log format: text or json")
	r.str(&cfg.debug, "debug", "comma-separated components to debug-log: all,"+strings.Join(components, ","))
	r.boolean(&cfg.verbose, "v", "debug-log every component (same as -debug=all)")

	r.group("Profiling (flame graphs)")
	r.add(&switchAddr{p: &cfg.profiling.addr, def: defaultFlameAddr}, "flame", "[=addr]",
		"serve Go pprof profiles over HTTP at /debug/pprof for flame graphs; -flame alone uses "+defaultFlameAddr+", -flame=:6060 listens on every interface", "")
	r.str(&cfg.profiling.cpuFile, "flame-cpu-file", "write a CPU profile of the whole run to this file at exit (Go allows one CPU profile at a time, so this disables -flame's /debug/pprof/profile)")
	r.str(&cfg.profiling.heapFile, "flame-heap-file", "write a heap profile to this file at exit")
	r.integer(&cfg.profiling.blockRate, "flame-block-rate", 0, "record blocking events lasting this many nanoseconds or more (runtime.SetBlockProfileRate); 0 is off, 1 records all", "")
	r.integer(&cfg.profiling.mutexFraction, "flame-mutex-fraction", 0, "sample 1 in this many mutex contention events (runtime.SetMutexProfileFraction); 0 is off", "")
	py := &cfg.profiling.pyroscope
	r.str(&py.url, "pyroscope-url", "upload continuous CPU and heap profiles to this Pyroscope server or Grafana Cloud Profiles URL; setting it turns uploads on. http:// or https:// decides TLS")
	r.str(&py.app, "pyroscope-app", "application name in Pyroscope; it appends the profile type (torrentai.cpu, ...)")
	r.list(&py.labels, "pyroscope-labels", "extra key=value labels on every profile, e.g. env=homelab; hostname, version and go_version are added")
	r.duration(&py.interval, "pyroscope-interval", time.Second, "length of each CPU profile, and how often profiles are uploaded", "")
	r.str(&py.user, "pyroscope-user", "basic auth user; for Grafana Cloud, the stack's profiles instance ID")
	r.str(&py.password, "pyroscope-password", "basic auth password; for Grafana Cloud, an access policy token with profiles:write. Never logged")
	r.secret("pyroscope-password")
	r.str(&py.tenant, "pyroscope-tenant", "X-Scope-OrgID tenant header, for multi-tenant servers")
	r.str(&py.caFile, "pyroscope-ca-file", "PEM CA certificate to trust, for a self-signed server")
	r.boolean(&py.insecureSkipVerify, "pyroscope-insecure-skip-verify", "don't verify the server's TLS certificate (logs a warning); prefer -pyroscope-ca-file")

	r.group("OpenTelemetry")
	r.boolean(&cfg.otel.enabled, "otel", "export traces, metrics and logs with OpenTelemetry; TORRENT_AI_OTEL turns it on just by being set. The flags below map to the standard OTEL_* variables, which apply when a flag is left empty")
	r.str(&cfg.otel.endpoint, "otel-endpoint", "OTLP endpoint, e.g. http://localhost:4318 (OTEL_EXPORTER_OTLP_ENDPOINT)")
	r.str(&cfg.otel.protocol, "otel-protocol", "OTLP protocol: grpc or http/protobuf (OTEL_EXPORTER_OTLP_PROTOCOL; default http/protobuf)")
	r.str(&cfg.otel.headers, "otel-headers", "OTLP headers as key=value,key=value, e.g. for an API key; never logged (OTEL_EXPORTER_OTLP_HEADERS)")
	r.secret("otel-headers")
	r.str(&cfg.otel.insecure, "otel-insecure", "true to use OTLP over gRPC without TLS (OTEL_EXPORTER_OTLP_INSECURE)")
	r.str(&cfg.otel.caFile, "otel-ca-file", "PEM CA certificate to trust for the OTLP endpoint, e.g. a self-signed homelab collector (OTEL_EXPORTER_OTLP_CERTIFICATE)")
	r.str(&cfg.otel.tracesExporter, "otel-traces-exporter", "otlp, console or none (OTEL_TRACES_EXPORTER; default otlp)")
	r.str(&cfg.otel.metricsExporter, "otel-metrics-exporter", "otlp, prometheus, console or none (OTEL_METRICS_EXPORTER; default otlp)")
	r.str(&cfg.otel.logsExporter, "otel-logs-exporter", "otlp, console or none (OTEL_LOGS_EXPORTER; default otlp)")
	r.str(&cfg.otel.serviceName, "otel-service-name", "service.name resource attribute (OTEL_SERVICE_NAME; default torrent-ai)")
	r.str(&cfg.otel.resourceAttrs, "otel-resource-attributes", "extra resource attributes as key=value,key=value (OTEL_RESOURCE_ATTRIBUTES)")
	r.str(&cfg.otel.sampleRatio, "otel-sample-ratio", "fraction of traces to keep, 0 to 1 (sets OTEL_TRACES_SAMPLER=parentbased_traceidratio; default keeps all)")
	r.duration(&cfg.otel.metricInterval, "otel-metric-interval", 0, "how often metrics are exported (OTEL_METRIC_EXPORT_INTERVAL; default 1m)", "")
	r.boolean(&cfg.aggregateMetrics, "otel-aggregate-torrents", "serve: sum torrent metrics across torrents instead of labeling each with torrent.infohash and torrent.name")

	r.group("Peers")
	r.list(&cfg.peers, "peer", "peer address (host:port) to connect to directly; repeatable or comma-separated")
	r.integer(&cfg.swarm.MaxPeers, "max-peers", 1, "most peer connections at once, incoming and outgoing", "")
	r.integer(&s.MaxInFlight, "max-requests", 1, "block requests kept in flight per peer", "")
	r.duration(&s.RequestTimeout, "request-timeout", time.Millisecond, "how long a block request may go unanswered before it is requested elsewhere", "")
	size(r, &s.BlockSize, "block-size", 1, "size of the blocks requested from peers", "BEP 3, 16KiB; peers commonly disconnect on larger requests")
	size(r, &s.MaxRequestSize, "max-request-size", 1, "largest block request served to peers", "")
	size(r, &s.MaxMemory, "max-memory", 0, "most memory held in pieces still being downloaded; 0 means no limit", "")
	r.integer(&s.BanAfter, "ban-after", 1, "failed pieces a peer may contribute to before it is banned (a peer that alone sent a failed piece is banned at once)", "")
	r.duration(&cfg.swarm.RetryBackoff, "retry-backoff", time.Millisecond, "how long a peer is skipped after a failed connection; doubles per failure", "")
	r.duration(&cfg.swarm.MaxBackoff, "max-retry-backoff", time.Millisecond, "longest a failing peer is skipped", "")
	r.duration(&s.Tick, "session-tick", time.Millisecond, "how often new peers are dialed and stale requests expired (also paces the metadata fetch)", "")

	r.group("Peer connections")
	r.duration(&conn.HandshakeTimeout, "handshake-timeout", time.Millisecond, "how long connecting and exchanging handshakes may take", "")
	r.duration(&conn.KeepAlive, "keepalive", time.Millisecond, "how long a connection may go without us writing before a keep-alive is sent", "BEP 3, about every 2 minutes")
	r.duration(&conn.ReadTimeout, "peer-read-timeout", time.Millisecond, "drop a peer that sends nothing, not even a keep-alive, for this long", "")
	r.duration(&conn.WriteTimeout, "peer-write-timeout", time.Millisecond, "how long a single write to a peer may take", "")
	r.integer(&conn.SendQueue, "send-queue", 1, "outgoing messages that may queue for a peer before it is dropped as too slow", "")
	size(r, &conn.MaxMessageSize, "max-message-size", 1, "largest message accepted from a peer", "")

	r.group("Metadata (magnet links)")
	r.integer(&cfg.metadata.Parallel, "metadata-parallel", 1, "peers tried at once for the info dict", "")
	r.duration(&cfg.metadata.Timeout, "metadata-timeout", time.Millisecond, "how long one peer gets to deliver a verified info dict", "")
	r.duration(&cfg.metadata.Heartbeat, "metadata-heartbeat", time.Millisecond, "how often \"metadata waiting\" is logged", "")
	r.integer(&meta.Window, "metadata-window", 1, "metadata piece requests kept in flight per peer", "")
	size(r, &meta.MaxSize, "max-metadata-size", 1, "largest info dict accepted", "")
	size(r, &meta.PieceSize, "metadata-piece-size", 1, "size of metadata pieces, requested and served", "BEP 9, 16KiB; other clients assume it")

	r.group("Trackers")
	r.boolean(&cfg.useTrackers, "trackers", "announce to the trackers in the .torrent or magnet link (-trackers=false to skip them)")
	r.list(&cfg.trackers, "tracker", "extra announce URL (http, https or udp); repeatable or comma-separated")
	r.duration(&cfg.trackerTimeout, "tracker-timeout", time.Millisecond, "how long an HTTP announce may take", "")
	r.integer(&cfg.numWant, "tracker-numwant", 0, "peers to ask each tracker for; 0 leaves it to the tracker", "")
	r.duration(&cfg.trackerMinWait, "tracker-min-interval", time.Millisecond, "serve: shortest wait between re-announces, whatever interval a tracker asks for", "")
	r.duration(&cfg.udpTimeout, "udp-tracker-timeout", time.Millisecond, "first UDP tracker timeout; attempt n waits this times 2^n", "BEP 15, 15s")
	r.integer(&cfg.udpRetries, "udp-tracker-retries", 0, "UDP tracker retries after the first attempt", "BEP 15, 8")
	r.duration(&cfg.udpConnTTL, "udp-connection-id-lifetime", time.Millisecond, "how long a UDP tracker connection ID is reused", "BEP 15, 1 minute")

	r.group("DHT")
	r.boolean(&cfg.useDHT, "dht", "find peers through the DHT (-dht=false to disable)")
	r.list(&cfg.dhtRouters, "dht-routers", "host:port nodes to bootstrap from; replaces the defaults, repeatable or comma-separated")
	r.duration(&cfg.dhtInterval, "dht-interval", time.Millisecond, "how often the DHT lookup and announce for the torrent repeats", "")
	r.duration(&cfg.dhtRetryInterval, "dht-retry-interval", time.Millisecond, "first retry after a weak bootstrap or a lookup that found no peers; doubles up to -dht-interval", "")
	r.duration(&cfg.dhtStatsInterval, "dht-stats-interval", time.Millisecond, "how often \"dht stats\" is logged", "")
	r.duration(&cfg.dhtSaveInterval, "dht-save-interval", time.Millisecond, "serve: how often the routing table is saved to the state dir (also saved at shutdown)", "")
	r.duration(&cfg.dht.QueryTimeout, "dht-query-timeout", time.Millisecond, "how long a DHT query may go unanswered", "")
	r.integer(&cfg.dht.K, "dht-k", 1, "bucket size, and nodes a lookup converges on and replies carry", "BEP 5, 8")
	r.integer(&cfg.dht.MaxFailures, "dht-max-failures", 1, "unanswered queries in a row before a node is considered bad", "")
	r.integer(&cfg.dht.Alpha, "dht-alpha", 1, "queries a lookup keeps in flight", "")
	r.integer(&cfg.dht.BootstrapAlpha, "dht-bootstrap-alpha", 1, "queries in flight while bootstrapping", "")
	r.duration(&cfg.dht.TokenRotation, "dht-token-rotation", time.Millisecond, "how often the token secret changes; tokens stay valid for twice this", "BEP 5, tokens up to 10 minutes old are accepted")
	r.duration(&cfg.dht.PeerTTL, "dht-peer-ttl", time.Millisecond, "how long peers announced to us are kept", "")
	r.integer(&cfg.dht.MaxValues, "dht-max-values", 1, "most peers in a get_peers reply", "")
	r.integer(&cfg.dht.MaxStoredPeers, "dht-max-stored-peers", 1, "most announced peers kept per infohash", "")
}

// validate checks values the flag types can't: address and URL
// formats. Hostnames are resolved later, when they're used.
func (cfg *config) validate() error {
	for _, h := range cfg.peers {
		if _, _, err := net.SplitHostPort(h); err != nil {
			return fmt.Errorf("-peer %q: %w", h, err)
		}
	}
	for _, h := range cfg.dhtRouters {
		if _, _, err := net.SplitHostPort(h); err != nil {
			return fmt.Errorf("-dht-routers %q: %w", h, err)
		}
	}
	for _, u := range cfg.trackers {
		if _, err := tracker.New(u); err != nil {
			return fmt.Errorf("-tracker %q: %w", tracker.Redact(u), err)
		}
	}
	if a := cfg.profiling.addr; a != "" {
		if _, _, err := net.SplitHostPort(a); err != nil {
			return fmt.Errorf("-flame %q: %w", a, err)
		}
	}
	if err := cfg.otel.validate(); err != nil {
		return err
	}
	if cfg.logFormat != "text" && cfg.logFormat != "json" {
		return fmt.Errorf("-log-format must be text or json, got %q", cfg.logFormat)
	}
	return nil
}

// peerSourceDHT returns the DHT loop settings.
func (cfg *config) peerSourceDHT() peersource.DHTConfig {
	return peersource.DHTConfig{
		Routers:       cfg.dhtRouters,
		MinNodes:      cfg.dht.K,
		Interval:      cfg.dhtInterval,
		RetryInterval: cfg.dhtRetryInterval,
	}
}

// finish copies settings that more than one package uses.
func (cfg *config) finish() {
	cfg.metadata.Tick = cfg.session.Tick
	cfg.metadata.Conn = cfg.session.Conn
	cfg.metadata.Metadata = cfg.session.Metadata
	cfg.session.Dir = cfg.dir
}
