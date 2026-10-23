# torrent-ai

A BitTorrent client written from scratch in Go. The BitTorrent protocol,
DHT and tracker code use only the standard library; the one runtime
dependency is the OpenTelemetry SDK, for optional tracing, metrics and
log export. It downloads from `.torrent` files and magnet links, finds
peers through HTTP and UDP trackers and the Mainline DHT, verifies every
piece, resumes interrupted downloads and seeds.

It was built by an AI agent, [Claude Code](https://claude.com/claude-code),
which also ran, debugged and verified it against real swarms and the live
DHT. The code is a working client; the more interesting part is how it
was developed.

## How it was developed

The human side of this project was a written plan
([PLAN.md](PLAN.md): scope, package layout, five phases with milestones),
a small starting skeleton (a bencoding decoder, a metainfo parser and an
HTTP tracker request), and a handful of short prompts, such as "time to
implement the plan", "please make the commits easy to review as
individual, small changes" and "I want this implementation to be agent
friendly". Everything after that was done by the agent: writing the code
and tests, running the binary against the real network, reading what
happened, and fixing what it found.

### Build and verify against the real network

Each phase of the plan ended with a milestone checked on the live
network rather than only in tests:

- **Download a `.torrent` over HTTP.** The Debian 13.7 netinst ISO
  (793 MB) downloads in about a minute, and its SHA-256 matches Debian's
  published sum.
- **UDP trackers.** Announces to public UDP trackers return peers.
- **Magnet links.** Metadata is fetched from real peers and checked
  against the infohash.
- **DHT.** A bare magnet link with no trackers joins the DHT, finds the
  peers, fetches the metadata and downloads the whole ISO, hash
  verified.

Offline, the tests run real sessions against each other on localhost:
seeders and leechers, peers sharing pieces, resuming from corrupted
data, fetching metadata from a peer, and a 30-node local DHT.

Work landed as small commits, each with its tests and a message
explaining what changed and why, so the history can be reviewed one step
at a time.

### Logs written for the agent

A program an agent runs against the real network is only as debuggable
as its output. So once the client worked, the agent went back through
the code and added logging meant to be read by an agent, not a person
watching a terminal:

- **Structured and stable.** `-log-format json` writes one object per
  line. Every line has a `component` (main, session, metadata, tracker,
  dht) and a fixed event name to match on, so `jq` can answer questions
  directly.
- **Runs that end on their own.** `-timeout` bounds a run, `-port 0` lets
  several runs go in parallel, and the last line is always `exit` with a
  result and a distinct exit code (complete, error, timeout,
  interrupted).
- **Summaries, not just events.** A `status` line every 5 seconds
  (progress, rates, peers unchoking us, requests in flight, dial and hash
  failures). DHT lookups report how many nodes answered and whether the
  lookup converged. A stuck metadata fetch logs a heartbeat that counts
  its failures by reason.
- **Detail on demand.** `-debug=session,dht` turns on per-peer and
  per-query events for just those components: why each peer
  disconnected, how long it stayed, what it sent.

[DEBUGGING.md](DEBUGGING.md) lists every event and its keys, `jq`
recipes, and how to get from a symptom to the log lines that explain it.
[LEARNINGS.md](LEARNINGS.md) collects what the logs, counters, traces
and profiles showed along the way.

### What the loop found

With those logs, the agent ran a live magnet download, read the JSON
output, and diagnosed it without a human looking at anything:

- **A peer sending bad data could stall a download forever.** The new
  counters showed one lying peer causing 143,000 hash failures and
  4.7 GB of traffic for a 300 KB torrent, because failed pieces were
  requested again from the same peer. Peers that send corrupt pieces
  are now banned.
- **Per-peer accounting was missing the most useful peers.** Peers still
  connected when a session stopped were never logged, so "who sent us
  the most data" only covered peers that had already left. After the
  fix, the per-peer totals in a live run add up to exactly the size of
  the torrent.
- **DHT bootstrap was slow.** Measuring it showed about 34 seconds,
  mostly unreachable nodes each holding a query slot until timeout.
  Widening the bootstrap lookup brought it down to 12-16 seconds.
- **Where a run spends its time.** For example, a magnet run's logs show
  the DHT bootstrap taking 12.7s (one public router timing out), metadata
  arriving at 16.8s, and the 793 MB download taking 46s at 17 MB/s with
  no hash failures.

## Install

Requires Go 1.26 or newer. To install the `torrentai` binary:

```sh
go install github.com/cnnrznn/torrent-ai/cmd/torrentai@latest
```

To work on the code, clone it and install from your copy. Run the same
`go install` again after each edit:

```sh
git clone https://github.com/cnnrznn/torrent-ai
cd torrent-ai
go install ./cmd/torrentai
```

Either way the binary lands in `$(go env GOPATH)/bin` (`~/go/bin` unless
`GOBIN` is set), which needs to be on your `PATH`. To build without
installing, use `go build ./cmd/torrentai`, which writes `./torrentai`.

## Usage

With no flags, a download goes to the current directory, using the DHT
and any trackers in the link, and the program exits once every piece is
verified:

```sh
torrentai 'magnet:?xt=urn:btih:<infohash>'
torrentai debian.iso.torrent

# The way an agent runs it: bounded, parallel-safe, machine-readable.
torrentai -timeout 5m -port 0 -log-format json -debug=metadata,session \
  -o /tmp/dl 'magnet:?xt=urn:btih:<infohash>' 2> run.jsonl
```

Quote magnet links: `?` and `&` mean something to the shell.

### The daemon

`torrentai serve` downloads and seeds many torrents in one process. They
share one peer port, one DHT node and caps on peers and memory. Torrents
survive restarts: they're saved in a state dir (`~/.local/state/torrentai`
by default) with their info dicts and the DHT routing table, so a restart
neither refetches metadata nor bootstraps from scratch.

```sh
torrentai serve -o ~/Downloads &          # API on <state-dir>/api.sock
torrentai add 'magnet:?xt=urn:btih:<infohash>'
torrentai add -o /srv/isos debian.iso.torrent
torrentai ls                              # -json for scripts
torrentai status <infohash>
torrentai rm -delete-files <infohash>
```

The API is HTTP/JSON (`POST/GET /v1/torrents`, `GET/DELETE
/v1/torrents/{infohash}`, `/healthz`, `/readyz`) on a unix socket, and
optionally on TCP with `-api-addr` and `-api-token`. Trackers are
re-announced for as long as a torrent runs, and `-seed-ratio` or
`-seed-time` stop seeding. [deploy/](deploy/k8s/README.md) has a
Dockerfile and Kubernetes manifests.

Every tunable parameter is a command-line flag. `torrentai -h` lists them
grouped by subsystem (general, logging, peers, peer connections,
metadata, trackers, DHT), with defaults:

- **Resources:** `-max-peers`, `-max-requests` per peer, and
  `-max-memory`, which caps RAM held in pieces still being downloaded.
- **Peer sources:** `-peer` to connect straight to a given client,
  `-tracker` to add trackers, `-trackers=false` and `-dht=false` to turn
  sources off, and `-dht-routers` to bootstrap elsewhere.
- **Timeouts, intervals and limits** for every subsystem.

Parameters a BEP defines (block size, keep-alive interval, metadata piece
size, the DHT's bucket size and token lifetime, UDP tracker retries) are
tunable too. They default to the spec-defined recommended value, are
marked as such in `-h`, and log a warning when changed.

Every flag can also be set from the environment as `TORRENT_AI_` plus
the flag name in upper case, with `-` as `_`: `TORRENT_AI_MAX_PEERS=20`.
Switches turn on just by being set, so `TORRENT_AI_FLAME=` enables
profiling. Flags on the command line take precedence.

Ctrl-C and SIGTERM (a container or service stop) both shut down
cleanly, writing profiles and flushing telemetry, and the next run
resumes from the verified pieces on disk.

## Observability

Both features are off by default and can be switched on from the
environment, so a deployment can enable them without changing its
command line.

**Flame graphs.** `TORRENT_AI_FLAME` (or `-flame`) serves Go's standard
`/debug/pprof` on `localhost:6060`; set it to an address such as `:6060`
to serve other hosts. Pyroscope, Parca or Grafana Alloy can scrape it
continuously, or pull a flame graph on demand:

```sh
TORRENT_AI_FLAME= torrentai 'magnet:?xt=urn:btih:<infohash>'
go tool pprof -http=: 'http://localhost:6060/debug/pprof/profile?seconds=30'
```

Goroutines carry a `component` label (session, dht, metadata, tracker),
so a flame graph can be split by subsystem (`-tagfocus` in pprof).
`-flame-cpu-file` and `-flame-heap-file` write profiles at exit instead,
for one-off runs.

**Continuous profiling.** `TORRENT_AI_PYROSCOPE_URL` (or
`-pyroscope-url`) uploads a CPU and a heap profile every minute to
Pyroscope or Grafana Cloud Profiles, and a final pair at shutdown. A
homelab server needs only the URL; Grafana Cloud takes
`-pyroscope-user` and `-pyroscope-password`, multi-tenant servers
`-pyroscope-tenant`, and a self-signed certificate `-pyroscope-ca-file`.
In the daemon each torrent's goroutines are also labeled with its
infohash, so flame graphs split per torrent.

**OpenTelemetry.** `TORRENT_AI_OTEL` (or `-otel`) exports traces, metrics
and logs, over OTLP to a collector by default:

```sh
TORRENT_AI_OTEL= TORRENT_AI_OTEL_ENDPOINT=http://localhost:4318 \
  torrentai 'magnet:?xt=urn:btih:<infohash>'
```

- **Traces:** one trace per run (per torrent in the daemon), with spans
  for tracker announces, DHT bootstrap and lookups, the metadata fetch
  and each attempt, the download session with a span per peer
  connection, and API requests.
- **Metrics:** progress, rates, peer counts, dial and hash failures,
  memory buffered and DHT traffic. The daemon labels them with
  `torrent.infohash` and `torrent.name`
  (`-otel-aggregate-torrents` sums them instead), plus torrents by state.
- **Logs:** the same events as stderr, with the same `-debug` filtering.

Exporters, protocol, endpoint, headers, sampling and resource
attributes are set with the `-otel-*` flags (`TORRENT_AI_OTEL_*`), or
with the standard `OTEL_*` variables when a flag is left empty. See
`torrentai -h`.

## What's implemented

| BEP | |
|---|---|
| [3](https://www.bittorrent.org/beps/bep_0003.html) | metainfo, HTTP trackers, peer wire protocol |
| [5](https://www.bittorrent.org/beps/bep_0005.html) | Mainline DHT: routing table, iterative lookups, announce, answering queries |
| [9](https://www.bittorrent.org/beps/bep_0009.html) | magnet links and metadata exchange |
| [10](https://www.bittorrent.org/beps/bep_0010.html) | extension protocol |
| [15](https://www.bittorrent.org/beps/bep_0015.html) | UDP trackers |

```
pkg/bencoding  bencode parser and encoder
pkg/metainfo   .torrent files and magnet links
pkg/tracker    HTTP and UDP tracker clients
pkg/peerwire   handshake, messages, connections, extension protocol, metadata
pkg/dht        KRPC, routing table, lookups, server
pkg/swarm      peer addresses and connection caps, shareable across torrents
pkg/torrent    download session: piece picker, verification, storage, memory budget
pkg/peersource tracker and DHT loops that find peers for a torrent
pkg/daemon     many torrents in one process: Manager, state store, HTTP API and client
pkg/pyroscope  continuous profile uploads
cmd/torrentai  the command: one-shot download, serve, and the API client commands
```

## Limitations

- Uploading has only been tested between this client's own sessions.
  Every live run was a download, so serving pieces and metadata to other
  clients, and answering their DHT queries, hasn't been checked against
  another implementation yet.
- Every interested peer is unchoked; there is no choking algorithm.
- The one-shot command announces to trackers once; the daemon
  re-announces. `announce-list` (BEP 12), PEX (BEP 11) and IPv6 aren't
  supported.
- The Kubernetes manifests are checked by a test but haven't been run in
  a cluster yet.

## Testing

```sh
go test -race ./...
```
