# Learnings from developing with logs, metrics, traces and profiles

Notes from building this client with an AI agent (Claude Code) that ran
the binary against the live BitTorrent network and debugged it from its
own output. Numbers are from real runs during development, mostly
downloads of the Debian 13.7 netinst ISO (793 MB, 3024 pieces).

## The order things happened in

1. The client was written and tested first, with almost no logging.
   Milestones were checked by downloading the ISO and comparing its
   SHA-256 with Debian's.
2. A logging pass then made the binary readable by an agent: JSON lines,
   stable event names, per-component debug, status lines, heartbeats,
   and a final `exit` line with a result and exit code.
3. Every tunable became a flag (57 of them), then settable from
   `TORRENT_AI_*` environment variables.
4. pprof profiling and OpenTelemetry (traces, metrics, logs) came last.

The order matters for reading the rest of this. **Most bugs were found
with structured logs and counters.** pprof and OTel arrived at the end;
they confirmed things and showed the shape of a run, but they didn't
find bugs on their own during this work.

## What the instrumentation found

| Symptom | Signal that showed it | Cause | Fix |
|---|---|---|---|
| A test with one lying peer sometimes never finished | `Stats.HashFailures` = 143,470 and `Downloaded` = 4.7 GB for a 300 KB torrent | Pieces that failed their hash were requested again, often from the same peer, forever | Ban a peer that alone sent a failed piece, or contributed to 3 |
| "Which peers sent us the most data" had nothing useful | jq over `peer closed` events: all 6 closed peers had `down=0` | Peers still connected at shutdown never logged `peer closed`, and those were the ones doing the work | Log every peer at shutdown; per-peer `down` now sums to exactly 792,723,456 bytes, the torrent's size |
| A magnet run sat for its whole 5-minute timeout | Info-level logs alone: `dht bootstrapped nodes=1 closest_bits=0`, then `metadata waiting known=0` every 10s | One router timed out, the other returned almost nothing; the client only re-bootstrapped an empty table, and the next lookup was 5 minutes away | Bootstrap until the table has k nodes; retry weak lookups after 5s, doubling |
| DHT bootstrap took ~34s | A throwaway probe program timing bootstrap | Unreachable nodes each held one of 3 query slots until timeout | Bootstrap 16 queries wide: 12-16s |
| `/debug/pprof/profile` returned an error mid-download | The live run itself: `cpu profiling already in use` | Go allows one CPU profile at a time, and `-flame-cpu-file` holds it for the whole run | A startup warning, and a note in `-h` |

Bugs found by tests rather than instrumentation, for completeness:
`-dht-routers` appended to the default routers instead of replacing
them; spans went missing on a second test run because package-level
tracers bind to the first global TracerProvider; and a DHT test hung
30s because a subtest's cleanup waited on a context only the parent
cancelled (found from per-subtest timings in `go test -v`).

## What the live network looked like

These weren't bugs, but they're what the logs showed and what a
developer of a BitTorrent client should expect:

- **`router.bittorrent.com` timed out in most runs.**
  `dht.transmissionbt.com` was usually enough to bootstrap alone.
- **Bootstrap is the slowest part of a magnet download.** In one traced
  run: bootstrap 22.4s, metadata at 23.5s, then the 793 MB download in
  47.1s.
- **Most dial failures are normal.** Of 88 in one run, 63 were TCP
  timeouts and 14 refused connections, NAT and firewalls.
- **Junk peers are common on the DHT.** Peers from runs of sequential
  ports on one subnet, with odd peer IDs, reset the connection at once.
  Not worth chasing.
- **The swarm is mostly qBittorrent.** About 30 of 47 connected peers
  in one run, then Transmission, libtorrent and Deluge.
- **The client is network-bound.** A whole-run CPU profile showed about
  16% of one core, ~78% of it socket syscalls and the network poller.
  SHA-1 piece checks were under 2%, the piece picker under 1%.
- **Memory is mostly partial pieces.** The heap profile showed ~24 MB
  (79% of in-use memory) in piece buffers, about one 256 KiB piece per
  active peer, which `-max-memory` caps. With an 8 MiB cap, buffered
  memory peaked at exactly 8 MiB and the download still completed, at
  11.7 MB/s instead of ~17.

## What made logs useful to an agent

- **Summaries beat streams of events.** The single most useful line was
  the metadata heartbeat: `metadata waiting known=0` says "no peer
  source has produced anything yet", which points straight at the DHT
  and trackers. A 5-second `status` line with counters did the same for
  downloads.
- **Counters make impossible numbers visible.** 4.7 GB downloaded for a
  300 KB torrent is obviously wrong in a way no individual event is.
- **Accounting identities are free checks.** Per-peer bytes should sum
  to the total. When they didn't, a logging gap showed up immediately.
- **Report whether something worked, not just that it ran.**
  `dht bootstrapped` with `nodes=1` and `closest_bits=0` was logged as
  success; the stall was visible only because the stats were on the
  line. `closest_bits` (how close a lookup got to its target, ~20+ when
  it converges) turned out to be the best single DHT health signal.
- **Classify failures.** Counting metadata failures by reason (dial,
  no_ut_metadata, rejected, bad_hash, timeout, closed) turns "it's not
  working" into a specific cause.
- **Stable event names, a component on every line, JSON output.** The
  agent matched on `msg`, never on error text, and filtered with jq.
  Durations are logged as `"12.69s"`, not nanoseconds.
- **Runs must end on their own and say how.** `-timeout`, `-port 0` for
  parallel runs, and a final `exit` line with a result and exit code
  meant every run could be launched unattended and read afterwards.
- **Log the effective configuration** (every flag, once, at startup),
  and warn when a spec-defined default is overridden.
- **Debug output per component.** `-debug=dht` logs every DHT query,
  far too much to read alongside peer events; scoping debug to one
  subsystem kept runs readable.
- **Redact as you go.** Tracker passkeys live in announce URL query
  strings, and OTLP headers usually carry API keys. Both are stripped
  from logs.

## When each signal helped

- **Logs and jq** were the main debugging tool. Every live diagnosis
  started with the info-level timeline, then a jq recipe over debug
  events.
- **Counters in `Stats`** found pathological behavior in tests that
  looked like a slow test rather than a bug.
- **Traces** showed the shape of a run at a glance: one trace with the
  bootstrap, the DHT announce running alongside the metadata fetch and
  its 8 attempts, and the session with 45 peer connections, each
  carrying its client, bytes and close reason. They'll matter more in a
  long-running daemon, where logs from many torrents interleave.
- **Profiles** answered one question decisively: is the client CPU-bound?
  It isn't. Goroutine labels by component showed 99% of labeled CPU in
  the session; the rest of the profile is Go runtime work outside any
  labels.
- **OTel metrics** mirror the status line (27 instruments), observed
  from the same snapshots so the two always agree. They become useful
  once there's a dashboard and more than one process.

## Pitfalls hit with OpenTelemetry and pprof

- **Don't store tracers in package variables.** A tracer obtained from
  the global provider before an SDK is installed keeps reporting to the
  first provider ever set. Look the tracer up when starting each span.
- **Go has one CPU profiler.** A whole-run CPU profile to a file and the
  HTTP endpoint's CPU profile can't run at the same time.
- **A CPU profile pulled at shutdown only shows shutdown.** To capture a
  whole run, the process has to record it itself (`-flame-cpu-file`) or
  be profiled continuously.
- **Profiling endpoints expose internals.** The default listen address
  is `localhost:6060`; serving other hosts is opt-in.
- **Flush telemetry on every exit path,** including after Ctrl-C, or the
  root span and final metrics are lost. SIGTERM isn't handled yet, so a
  container stop still loses them.
- **`autoexport` is convenient but heavy.** It picks exporters from the
  standard `OTEL_*` variables, and in exchange pulls in gRPC and the
  Prometheus exporter, about 40 modules.

## What I'd do differently

- **Instrument from the start.** The corrupt-peer stall was present from
  the first version of the download session; it was found only when
  counters were added, a dozen commits later. Counters and a status
  line cost little and catch this class of bug early.
- **Test against another implementation.** Every loopback test ran this
  client against itself, so a misreading of the spec on both sides
  would pass. Downloading from real swarms checked our side of the
  protocol, but uploading, serving metadata and answering DHT queries
  have still never been checked by another client.
- **Verify milestones end to end.** Comparing the downloaded ISO with
  Debian's published SHA-256 was the strongest single check in the
  project and is worth keeping for every change to the download path.

## Still open

- Handle SIGTERM like Ctrl-C, so container stops flush telemetry and
  write profiles.
- An interop test against a reference client, in both directions.
- A long-running daemon serving many torrents, which would keep the DHT
  warm and suit continuous profiling better than one process per
  download.
