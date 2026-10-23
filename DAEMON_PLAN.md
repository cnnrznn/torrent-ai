# Daemon plan

## Goal

Run torrentai as a long-lived service that downloads and seeds many
torrents in one process, controlled over an API, and observable in
production: continuous flame graphs in Pyroscope, plus the OTel traces,
metrics and logs that already exist.

Why a daemon rather than one process per download:

- **A warm DHT.** Every one-shot run spends 10-20s bootstrapping. A
  long-lived node keeps its routing table, and can save it across
  restarts.
- **Shared resources.** One listening port, one DHT node, and global
  caps on peers and memory, instead of each process competing.
- **Continuous profiling.** A long-lived process is what Pyroscope is
  built for. The daemon pushes profiles itself, so no sidecar is needed.

The one-shot `torrentai <magnet>` stays, unchanged.

## Scope

In scope: multi-torrent sessions sharing one listener and DHT node, an
HTTP/JSON control API, a CLI client for it, persistent state with resume
after restart, tracker re-announces, saving the DHT routing table,
SIGTERM handling, health endpoints, a built-in Pyroscope push, and
example Kubernetes manifests.

Out of scope: a web UI, a choking algorithm (still unchoke everyone),
PEX (BEP 11), IPv6, and protocol encryption (MSE).

## Command layout

One binary with subcommands, so there's still a single `go install`:

```
torrentai <magnet|file>             one-shot download (today's behaviour)
torrentai serve [flags]             run the daemon
torrentai add <magnet|file> [-o dir]
torrentai ls
torrentai status <infohash>
torrentai rm <infohash> [-delete-files]
```

The client subcommands talk to the daemon's API. Every flag keeps a
`TORRENT_AI_*` variable, as today.

## Architecture

```
                ┌──────────────────── torrentai serve ────────────────────┐
API (unix sock  │  Manager ── sessions[infohash] ── torrent.Session ...   │
 or TCP+token) ─┤     │                                                    │
                │     ├─ shared TCP listener ─ route by infohash ─┐        │
                │     ├─ one dht.Server (routing table saved)     │        │
                │     ├─ global peer and memory caps              │        │
                │     ├─ tracker re-announce loops                ▼        │
                │     └─ state dir (torrents, info dicts, DHT)  sessions   │
                │  pprof + Pyroscope push, OTel, /healthz /readyz          │
                └──────────────────────────────────────────────────────────┘
```

### pkg/daemon (new): Manager

Owns the shared pieces and the set of sessions. `Add(source, dir)`
parses a magnet or .torrent, fetches metadata if needed, and starts a
session. `Remove`, `List` and `Status` back the API. One goroutine per
session, as today; the Manager itself is a small mutex-guarded map.

### Changes to existing packages

- **peerwire: route incoming connections by infohash.** Today
  `Accept(nc, local)` reads the remote handshake and rejects any
  infohash but one. Add an accept that reads the handshake first, asks a
  lookup function for the matching session, and answers with that
  session's handshake.
- **torrent: sessions without their own listener.** Replace
  `Config.Listener` with a `Session.AddConn(*peerwire.Conn)` the Manager
  calls for routed connections. The one-shot command keeps its own
  listener and routes to its single session the same way.
- **swarm: global limits.** A shared limiter caps connections across all
  swarms (`-max-peers` becomes daemon-wide), with an optional
  per-torrent cap. `-max-memory` likewise becomes a shared budget.
- **dht: save and load the routing table.** Write good nodes to the
  state dir periodically and on shutdown; seed the table from them at
  startup and fall back to the routers. Target: usable DHT within ~2s of
  a restart.
- **tracker re-announce.** Long-lived torrents announce every tracker
  `interval` (with a floor), and send `completed` and `stopped` events.
  This was out of scope for the one-shot client; it isn't for a daemon.

### State

A state dir (`-state-dir`, default `$XDG_STATE_HOME/torrentai`) holds
one JSON file per torrent (source, download dir, added time, whether
it's complete), its info dict so a restart never refetches metadata,
and the saved DHT nodes. On startup each torrent resumes by verifying
its files, as sessions already do. Saving the verified bitfield, to
skip re-hashing, is a later optimisation.

### Control API

HTTP/JSON. By default on a unix socket in the state dir, so access is
controlled by file permissions; optionally on TCP with a bearer token
(`-api-addr`, `-api-token`).

```
POST   /v1/torrents            {"magnet": "..."} or {"torrent": "<base64>"}, "dir"
GET    /v1/torrents            list with progress
GET    /v1/torrents/{infohash} status: pieces, rates, peers, trackers, DHT
DELETE /v1/torrents/{infohash} ?delete_files=true
GET    /healthz                process is up
GET    /readyz                 listener bound and DHT has at least k nodes
```

### Shutdown

SIGTERM and SIGINT both: stop accepting API calls, announce `stopped`
to trackers, save state and the DHT table, end sessions, push final
profiles, flush OTel, exit 0. This also fixes the one-shot command,
which today dies on SIGTERM without writing profiles or flushing
telemetry.

## Observability

- **Logs** keep their components; every session line gains `infohash`
  and `name`, so one torrent's activity can be filtered out of many.
- **Metrics** stay as today, with a `torrent.infohash` attribute per
  session. The number of torrents is bounded by what the operator adds,
  so cardinality is controlled; a flag can aggregate instead.
- **Traces:** a span per torrent's lifetime replaces the per-run root
  span, with the same children (metadata fetch, peer connections), plus
  a span per API request.
- **Flame graphs:** a built-in Pyroscope uploader (`-pyroscope-url`,
  `-pyroscope-interval`, default 60s) records a CPU profile per interval
  and a heap snapshot, and pushes them to Pyroscope's `/ingest` with
  `service`, `pod` and `version` labels. A final push runs at shutdown.
  The pprof endpoint stays for on-demand use and for Alloy scraping.
  The exact ingest request (parameters, profile formats, auth) gets
  checked against Pyroscope's documentation when implemented.

### Telemetry auth and TLS

Telemetry has to work both with a hosted service and with an
unprotected Grafana or Pyroscope on a homelab. Auth and TLS are
independent choices, for both the Pyroscope uploader and OTLP:

| Setup | Auth | TLS |
|---|---|---|
| Homelab or dev | none | optional: `http://`, or `https://` |
| Self-signed homelab cert | none or basic | a CA file (`-pyroscope-ca-file`, `-otel-ca-file`), or skip verification as an explicit opt-in that logs a warning |
| Grafana Cloud | basic: the product's instance ID and a Cloud Access Policy token with write scopes only | system CAs |
| Self-hosted multi-tenant | tenant header (`X-Scope-OrgID`), with or without basic | either |

The scheme of the URL decides TLS. With no credentials configured,
nothing is sent. Credentials come from flags or `TORRENT_AI_*`
variables (a Kubernetes Secret in production) and are redacted from
logs. `-otel-ca-file` maps to the standard
`OTEL_EXPORTER_OTLP_CERTIFICATE`.

GitHub OAuth isn't an option for this: Grafana uses it to log people
into the web UI, not to authenticate data sent to ingestion endpoints.
If keyless workload identity matters later, check what Grafana Cloud
supports at that point and add it as another auth mode.

## Phases

Each phase lands as small commits with tests, as before.

### Phase 0: SIGTERM

Handle SIGTERM like Ctrl-C in the current command: write profiles,
flush telemetry, log the `exit` line. Small and useful on its own.

### Phase 1: Many torrents, one process

Infohash routing in peerwire, `Session.AddConn`, the Manager, and
global caps. **Milestone:** a test with two torrents seeding and
leeching through one listener and one DHT node on localhost, each
finishing with correct data.

### Phase 2: Daemon, API and client

`torrentai serve`, the API on a unix socket, the client subcommands,
and the state dir. **Milestone:** add the Debian magnet through the
API, restart the daemon mid-download, and it resumes and finishes with
the right SHA-256, without refetching metadata.

### Phase 3: Long-running behaviour

Tracker re-announce with events, saving and loading the DHT table,
optional seeding limits (ratio or time). **Milestone:** after a restart
the DHT is ready in about 2s instead of 10-20s, measured from
`/readyz` and the `dht bootstrapped` log.

### Phase 4: Production observability

Per-torrent log, metric and span attributes, health endpoints, and the
Pyroscope uploader. **Milestone:** a 30-minute run with two torrents
shows continuous flame graphs in a local Pyroscope, split by component
label.

### Phase 5: Deployment

A Dockerfile and Kubernetes manifests: a StatefulSet with a volume for
state and downloads, a Service for the API, readiness and liveness
probes, a termination grace period long enough for the shutdown
sequence, and notes on exposing the peer port (TCP and UDP) for
incoming connections. **Milestone:** the Phase 2 restart test, run as
a pod replacement in a local cluster (kind).

## Decisions

1. **Subcommands in one binary**, not a separate `torrentaid`.
2. **API access:** a unix socket by default, with optional TCP and a
   bearer token.
3. **Per-torrent metric attributes** on by default, with a flag to
   aggregate.
4. **Seeding:** seed until removed; ratio and time limits are optional.
5. **Telemetry auth:** none, basic, or a tenant header, with optional
   TLS and a custom CA, so both Grafana Cloud and an unprotected homelab
   instance work. See "Telemetry auth and TLS".
