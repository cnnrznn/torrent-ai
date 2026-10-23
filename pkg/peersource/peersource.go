// Package peersource runs the loops that find peers for a torrent, from
// trackers and the DHT, and add them to its swarm.
package peersource

import (
	"context"
	"log/slog"
	"net/netip"
	"time"

	"github.com/cnnrznn/torrent-ai/pkg/dht"
	"github.com/cnnrznn/torrent-ai/pkg/id"
	"github.com/cnnrznn/torrent-ai/pkg/swarm"
	"github.com/cnnrznn/torrent-ai/pkg/tracker"
)

// Tracker announces once to the tracker at url and adds the peers it
// returns to sw.
func Tracker(ctx context.Context, log *slog.Logger, sw *swarm.Swarm, url string, opts []tracker.Option, req tracker.Request) {
	t, err := tracker.New(url, opts...)
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

// DHTConfig tunes the DHT loop.
type DHTConfig struct {
	// Routers are the host:port nodes to bootstrap from.
	Routers []string
	// MinNodes is the routing table size below which the loop
	// bootstraps again; the DHT's k.
	MinNodes int
	// Interval is how often a healthy lookup and announce repeats.
	Interval time.Duration
	// RetryInterval is the first retry after a weak bootstrap or a
	// lookup that found no peers; it doubles up to Interval.
	RetryInterval time.Duration
}

// DHT keeps node bootstrapped and repeatedly looks up and announces
// infoHash (accepting peers on port), adding the peers it finds to sw,
// until ctx is cancelled.
func DHT(ctx context.Context, log *slog.Logger, node *dht.Server, sw *swarm.Swarm, infoHash id.Hash, port int, cfg DHTConfig) {
	retry := cfg.RetryInterval
	for {
		// A table with fewer than k nodes can't support a useful
		// lookup, so keep bootstrapping until it fills.
		if node.Nodes() < cfg.MinNodes {
			st, err := node.Bootstrap(ctx, cfg.Routers)
			if ctx.Err() != nil {
				return
			}
			switch {
			case err != nil:
				log.Warn("dht bootstrap failed", "err", err, "nodes", node.Nodes(), "lookup", st)
			case node.Nodes() < cfg.MinNodes:
				log.Warn("dht bootstrap weak", "nodes", node.Nodes(), "want", cfg.MinNodes, "lookup", st)
			default:
				log.Info("dht bootstrapped", "nodes", node.Nodes(), "lookup", st)
			}
		}

		st, err := node.Announce(ctx, infoHash, port, func(peers []netip.AddrPort) {
			sw.Add(peers...)
		})
		if ctx.Err() != nil {
			return
		}

		var wait time.Duration
		wait, retry = nextLookup(node.Nodes() >= cfg.MinNodes && err == nil && st.Peers > 0, retry, cfg)
		if err != nil {
			log.Warn("dht lookup failed", "infohash", infoHash.Hex(), "err", err, "lookup", st, "next", wait)
		} else {
			log.Info("dht lookup done", "infohash", infoHash.Hex(), "nodes", node.Nodes(), "lookup", st, "next", wait)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// nextLookup decides how long to wait before the next DHT lookup. A
// healthy lookup waits the full interval. Otherwise (too few nodes, an
// error, or no peers) it retries after retry, doubling the retry each
// time up to the interval. It returns the wait and the retry to use
// next time.
func nextLookup(healthy bool, retry time.Duration, cfg DHTConfig) (wait, nextRetry time.Duration) {
	if healthy {
		return cfg.Interval, cfg.RetryInterval
	}
	return retry, min(2*retry, cfg.Interval)
}
