# torrent-ai

A BitTorrent client written from scratch in Go, with no dependencies
outside the standard library (testify is used in tests). It downloads
from `.torrent` files and magnet links, finds peers through HTTP and UDP
trackers and the Mainline DHT, verifies every piece, resumes interrupted
downloads and seeds.

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

## Usage

```sh
go build -o torrent ./cmd/base

./torrent -o ~/Downloads debian.iso.torrent
./torrent -o ~/Downloads 'magnet:?xt=urn:btih:<infohash>'

# The way an agent runs it: bounded, parallel-safe, machine-readable.
./torrent -timeout 5m -port 0 -log-format json -debug=metadata,session \
  -o /tmp/dl 'magnet:?xt=urn:btih:<infohash>' 2> run.jsonl
```

Flags: `-o` download directory, `-port` TCP/UDP port (0 picks a free
one), `-dht=false` to use trackers only, `-seed` to keep seeding after
completion, `-timeout`, `-log-format`, `-debug`, `-v`. Ctrl-C shuts down
cleanly, and the next run resumes from the verified pieces on disk.

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
pkg/swarm      peer addresses and the connection cap
pkg/torrent    download session: piece picker, verification, storage
cmd/base       the command-line client
```

## Limitations

- Uploading has only been tested between this client's own sessions.
  Every live run was a download, so serving pieces and metadata to other
  clients, and answering their DHT queries, hasn't been checked against
  another implementation yet.
- Every interested peer is unchoked; there is no choking algorithm.
- Trackers are announced to once per run, and `announce-list`
  (BEP 12), PEX (BEP 11) and IPv6 aren't supported.

## Testing

```sh
go test -race ./...
```
