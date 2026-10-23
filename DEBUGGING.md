# Running and debugging

The CLI is built to be run and diagnosed from its logs alone, by a
person or an agent, against real magnet links and the live DHT.

## Running

```sh
go build -o torrent ./cmd/base

./torrent -timeout 5m -port 0 -log-format json -debug=metadata,session \
  -o /tmp/dl 'magnet:?xt=urn:btih:<infohash>' 2> run.jsonl
echo $?
```

- Logs go to stderr; nothing is written to stdout.
- `-timeout` makes the run end by itself. `-port 0` picks a free port, so
  runs can go in parallel.
- `-log-format json` writes one JSON object per line. Durations are
  strings like `"12.69s"`.
- `-debug=<components>` turns on debug events for some components
  (`main`, `session`, `metadata`, `tracker`, `dht`, or `all`). `-v` is
  the same as `-debug=all`. `-debug=dht` is very chatty on the live DHT
  (one event per query).
- `-seed` keeps running after completion.
- Every tunable parameter is a flag; `./torrent -h` lists them by
  subsystem with their defaults. Flags whose default comes from a BEP are
  marked "spec-defined recommended default"; changing one logs a
  `spec default overridden` warning.

Flags that are useful when debugging:

- Peer sources: `-peer host:port` connects straight to a given client
  (repeatable), which is handy for interop testing. `-tracker URL` adds
  a tracker. `-trackers=false` skips the torrent's own trackers, and
  `-dht=false` disables the DHT, so one source can be tested at a time.
  `-dht-routers` bootstraps from your own nodes.
- Faster feedback: `-status-interval`, `-metadata-heartbeat`,
  `-dht-stats-interval`, `-dht-retry-interval`, and short timeouts
  such as `-request-timeout` and `-dht-query-timeout`.
- Resource limits: `-max-peers`, `-max-requests`, `-max-memory`.

A good test torrent is Debian's netinst image. It has a large swarm, and
SHA-256 sums are published for checking the result. The current
`.torrent` and `SHA256SUMS` are at
<https://cdimage.debian.org/debian-cd/current/amd64/bt-cd/>; a magnet
link needs only the infohash (the `loaded torrent` event shows it).

### Exit codes

The last line is always `exit`, with `result`, `elapsed` and `code`.

| code | result      | meaning                                  |
|------|-------------|------------------------------------------|
| 0    | complete    | every piece downloaded and verified      |
| 1    | error       | see `err` on the exit line               |
| 2    |             | bad flags (message on stderr, no `exit`) |
| 3    | timeout     | `-timeout` elapsed first                 |
| 130  | interrupted | SIGINT                                   |

## Events

Every line has `component` and `msg`. `msg` is a stable event name;
match on it, not on the text of `err`.

### main

| event | level | keys |
|---|---|---|
| `starting` | info | target |
| `config` | info | every flag's effective value, keyed by flag name |
| `spec default overridden` | warn | flag, value, default, spec |
| `no peer sources` | warn | hint (DHT and trackers disabled, no -peer or -tracker) |
| `listening` | info | port (the real port), dht |
| `fetching metadata` | info | name, infohash, trackers, direct_peers |
| `loaded torrent` | info | name, size, pieces, infohash |
| `status` | info | every `-status-interval` (5s), see below |
| `seeding` | info | |
| `exit` | info/error | result, elapsed, code, err |

`status` keys: `phase` (downloading, seeding), `pieces`/`of`/`percent`,
`left`, `down`/`up` (bytes) and `down_rate`/`up_rate` (bytes/s over the
last status line), `peers` connected, `unchoked_us` (peers letting us
request), `interested` (peers with pieces we need), `requests` in
flight, `buffered` (bytes held in pieces still downloading, capped by
`-max-memory`), `known` addresses, `banned` IPs, `dials`/`dial_failures`, `accepted` incoming,
`hash_failures`, `request_timeouts`, `dht_nodes`.

### metadata (magnet links only)

| event | level | keys |
|---|---|---|
| `metadata waiting` | info | every 10s: elapsed, known, in_flight, attempts, `failed_<reason>` counts |
| `metadata fetched` | info | from, size, name, attempts, dur |
| `metadata attempt` | debug | addr, result (ok, failed), reason, err, dur |

Failure reasons: `dial` (couldn't connect or handshake), `no_extensions`,
`no_ut_metadata`, `rejected`, `bad_hash`, `timeout`, `closed`, `protocol`.

### session

| event | level | keys |
|---|---|---|
| `session ready` | info | name, pieces, have, left, resumed |
| `verified existing data` | info | have, pieces, dur |
| `download complete` | info | downloaded, hash_failures, dur, avg_rate |
| `piece hash failed` | warn | piece, from (addresses that sent its blocks) |
| `peer banned` | warn | addr, client, reason, strikes, sole_source |
| `storage read failed` / `storage write failed` | error | piece, err |
| `listener accept failed` | warn | err |
| `peer connected` | debug | addr, incoming, client, extensions, dht, peers |
| `peer extensions` | debug | addr, client, v, ut_metadata, metadata_size |
| `peer bitfield` | debug | addr, client, has, pieces |
| `peer unchoked us` / `peer choked us` | debug | addr, client, dropped_requests |
| `peer requests timed out` | debug | addr, client, expired, choked |
| `peer protocol error` | debug | addr, client, msg, err |
| `peer closed` | debug | addr, incoming, client, err, connected, down, up, had |
| `peer dial failed` / `peer accept failed` | debug | addr, err |

`client` is decoded from the peer ID: `qB4630` is qBittorrent 4.6.3,
`TR4130` Transmission, `lt` libtorrent, `DE` Deluge.

### tracker

| event | level | keys |
|---|---|---|
| `tracker announce done` | info | url, peers, interval, dur |
| `tracker announce failed` | warn | url, err, dur |
| `tracker invalid` | warn | url, err |
| `tracker request` / `tracker response` | debug | url, event, left / status, bytes, peers, interval, dur |
| `udp tracker connect` / `announce` / `timeout` / `response` | debug | host, attempt, timeout, step |

Tracker URLs are logged without their query string, where private
trackers put passkeys.

### dht

| event | level | keys |
|---|---|---|
| `dht bootstrapped` | info | nodes, lookup |
| `dht bootstrap weak` | warn | nodes, want (k), lookup |
| `dht bootstrap failed` | warn | err, nodes, lookup |
| `dht lookup done` | info | infohash, nodes, lookup, next |
| `dht lookup failed` | warn | err, lookup, next |
| `dht stats` | info | every `-dht-stats-interval` (1m): nodes, buckets, stored_peers, sent, replies, error_replies, timeouts, received, announced_to_us, bad_packets |
| `dht router` | warn/debug | router, addr, result, nodes, err, dur |
| `dht query` | debug | method, to, result (ok, timeout, error), code, msg, dur |
| `dht query received` | debug | method, from |
| `dht peer announced` | debug | infohash, peer |
| `dht add node` | debug | addr, result, id, nodes |
| `dht bad packet` / `dht send failed` | debug | from/to, err |

`lookup` is a group: `queried`, `responded`, `failed`, `peers` (distinct),
`announced`, `dur`, and `closest_bits`, the number of leading bits the
closest answering node shares with the target. On the live DHT a lookup
that converged reaches about 20 or more.

`next` is when the next lookup runs. A healthy lookup waits
`-dht-interval` (5m). After a weak bootstrap (fewer than `-dht-k` nodes),
a failure or a lookup with no peers, it retries after
`-dht-retry-interval` (5s), doubling up to `-dht-interval`.

## Recipes

```sh
# Event counts, to see what happened at a glance.
jq -r '[.component, .level, .msg] | join(" ")' run.jsonl | sort | uniq -c | sort -rn

# The run without debug noise or status lines.
jq -c 'select(.level != "DEBUG" and .msg != "status") | del(.time)' run.jsonl

# Download progress over time.
jq -c 'select(.msg == "status") | {time, percent, down_rate, peers, unchoked_us, requests}' run.jsonl

# Why connections failed (addresses collapsed).
jq -r 'select(.msg == "peer dial failed") | .err | sub("[0-9.:]+[0-9]"; "ADDR"; "g")' run.jsonl | sort | uniq -c | sort -rn

# Peers that sent us the most data. Every peer logs "peer closed" when
# it disconnects or the session stops (err "session stopped").
jq -s -c 'map(select(.msg == "peer closed")) | sort_by(-.down) | .[:10][] | {addr, client, down, connected, err}' run.jsonl

# Which clients we talked to.
jq -r 'select(.msg == "peer connected") | .client' run.jsonl | sort | uniq -c | sort -rn
```

## Symptoms

**Stuck fetching metadata.** Read `metadata waiting`.
- `known=0`: no peer source has produced addresses. Look for
  `dht bootstrapped` (or `dht bootstrap weak`, `dht bootstrap failed`
  and `dht router` warnings) and `tracker announce failed`. Bootstrap
  often takes 10-20s, and `router.bittorrent.com` regularly times out;
  `dht.transmissionbt.com` is usually enough. A weak bootstrap retries
  on its own (see `next`); `-dht-routers` can point at other nodes.
- `known` > 0 but no success: the `failed_<reason>` counts say why.
  Mostly `dial` is normal (NAT, firewalls); mostly `no_ut_metadata` or
  `rejected` means the peers can't serve metadata.

**DHT finds no peers.** Check `dht lookup done`: `closest_bits` far below
20 means the lookup didn't converge (few `responded`, many `failed`;
check `timeouts` in `dht stats`). `peers=0` with good convergence
usually means the torrent has no DHT peers.

**Slow download.** In `status`: few `peers` with many `dial_failures`
means connections are failing (see the dial recipe). `peers` high but
`unchoked_us` low means peers are choking us. `requests` near zero
while `unchoked_us` > 0 means we have nothing to request from them
(`interested` low). Growing `request_timeouts` means peers aren't
answering. `buffered` sitting at `-max-memory` means the memory cap is
what's limiting the download.

**Hash failures.** `piece hash failed` names the senders. A peer that
alone sent a failed piece, or contributed to 3, is banned (`peer banned`),
and the piece is downloaded again from others.

**Peers dropping instantly.** `peer closed` with `connected` near 0 and
`down=0` is usually the peer's choice (connection limits, or junk peers
on the DHT). Many of these from runs of sequential ports on one subnet
are not worth chasing.
