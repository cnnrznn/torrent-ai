# Implementation plan

## Scope

| BEP | What |
|---|---|
| 3  | Base spec: metainfo, HTTP tracker, peer wire |
| 15 | UDP tracker |
| 5  | DHT (the dynamic peer source) |
| 9 + 10 | Magnet links + `ut_metadata` (10 is required by 9) |

Out of scope: announce-list (BEP 12), PEX (BEP 11), tracker re-announce
loops, fast extension, IPv6.

Note: the DHT only returns peers for an infohash. Metadata is fetched from
those peers over the wire protocol (BEP 9).

## Package layout

```
pkg/bencoding  decoder (existing) + encoder
pkg/metainfo   + FromInfoDict, magnet.go
pkg/tracker    http.go (existing), udp.go
pkg/peerwire   handshake, messages, Conn, extension.go (BEP 10), metadata.go (BEP 9)
pkg/dht        krpc, routing table, iterative lookup, server
pkg/swarm      address set + connection cap, fed by tracker and DHT
pkg/torrent    session: piece picker, verification, storage
```

## Phase 0: Foundations

### bencoding
- Bounds checks: empty or truncated input returns an error instead of
  panicking (`ParseInteger`, `ParseList`, `ParseDictionary`, `typeNext`, and
  the missing EOF check in `ParseDictionary`).
- Strict integers per BEP 3: allow only `0` or `-?[1-9][0-9]*`. Reject
  `i-0e`, `i03e`, `i+3e`, `ie`. String length prefixes: digits only, no
  leading zeros except `0:`.
- Encoder, in the same style as the decoder (values passed in are already
  encoded):
  ```go
  func EncodeString(s string) string
  func EncodeInteger(n int) string
  func EncodeList(items ...string) string
  func EncodeDictionary(m map[string]string) string // keys sorted
  ```
- Tests: bad and truncated input, strict integers, encode→parse round trip,
  and a fuzz test that must never panic.

### Other fixes
- tracker: send `left` and `event=started`; check `len(peers) % 6`.
- metainfo: return errors; add `FromInfoDict(raw string)`.
- Add `type Hash [20]byte` for infohash, peer ID and node ID. Use
  `netip.AddrPort` for peer addresses.
- Remove the duplicate `genPeerID`.

## Phase 1: Peer wire (BEP 3)
- Handshake. Set reserved bits for extensions (`0x10` in byte 5) and DHT
  (`0x01` in byte 7).
- Length-prefixed messages: choke, unchoke, interested, not interested,
  have, bitfield, request, piece, cancel, port, extended (id 20).
- `Conn` with a read goroutine; writes go through a channel.

## Phase 2: Download from a `.torrent` file
- Simple `swarm`: a set of addresses plus a connection cap.
- Piece picker. Request 16 KiB blocks with several in flight per peer.
- Check each piece's SHA-1, send `have`, write pieces to file(s).
- **Milestone:** download a Debian ISO over HTTP.

## Phase 3: UDP tracker (BEP 15)
- Connect, then announce, with transaction IDs and the spec's retry
  timeouts.
- Pick HTTP or UDP from the announce URL scheme.

## Phase 4: Magnet + metadata (BEP 9/10)
- Parse magnets: `xt=urn:btih:` in hex (40 chars) or base32 (32 chars),
  plus `dn` and `tr`.
- Extension handshake: `{m: {ut_metadata: N}, metadata_size: S}`.
- Fetch 16 KiB metadata pieces (msg_type 0 = request, 1 = data,
  2 = reject). A data message is a bencoded dict followed by raw bytes.
- Check `sha1(info) == infohash`, then call `metainfo.FromInfoDict`.
- Test with the magnet's `tr=` trackers, before the DHT exists.

## Phase 5: DHT (BEP 5)
- KRPC over UDP. Match replies by transaction ID, with timeouts.
- Routing table: XOR distance, k-buckets (k=8), split the bucket that
  contains our own ID.
- Bootstrap from `router.bittorrent.com:6881` and
  `dht.transmissionbt.com:6881`, then `find_node` on our own ID.
- Iterative `get_peers` with 3 queries in flight. Stream peers into `swarm`
  as they arrive. Repeat the lookup periodically.
- `announce_peer` to the nodes that returned tokens.
- Answer incoming `ping`, `find_node` and `get_peers`.
- Handle the wire `port` message by adding the peer's node to the table.
- **Milestone:** a magnet with no `tr=` downloads fully.

## Concurrency model
One goroutine per torrent session owns its state (picker, bitfields, peer
set). Connections and peer sources talk to it through channels. Shutdown is
done by cancelling a `context.Context`.
