package peersource

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/cnnrznn/torrent-ai/pkg/bencoding"
	"github.com/cnnrznn/torrent-ai/pkg/dht"
	"github.com/cnnrznn/torrent-ai/pkg/id"
	"github.com/cnnrznn/torrent-ai/pkg/swarm"
	"github.com/cnnrznn/torrent-ai/pkg/tracker"
)

var discard = slog.New(slog.DiscardHandler)

func TestNextLookup(t *testing.T) {
	cfg := DHTConfig{RetryInterval: 5 * time.Second, Interval: 30 * time.Second}

	// Unhealthy lookups retry sooner, backing off up to the interval.
	retry := cfg.RetryInterval
	var waits []time.Duration
	for range 5 {
		var wait time.Duration
		wait, retry = nextLookup(false, retry, cfg)
		waits = append(waits, wait)
	}
	require.Equal(t, []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 30 * time.Second, 30 * time.Second}, waits)

	// A healthy lookup waits the full interval and resets the backoff.
	wait, retry := nextLookup(true, retry, cfg)
	require.Equal(t, 30*time.Second, wait)
	require.Equal(t, 5*time.Second, retry)
}

func TestTracker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(bencoding.EncodeDictionary(map[string]string{
			"interval": bencoding.EncodeInteger(60),
			"peers":    bencoding.EncodeString("\x0a\x00\x00\x01\x1a\xe1"),
		})))
	}))
	defer srv.Close()

	sw := swarm.New(swarm.Config{})
	Tracker(context.Background(), discard, sw, srv.URL+"/announce", nil, tracker.Request{InfoHash: id.Random(), PeerID: id.NewPeerID()})
	addr, ok := sw.Next()
	require.True(t, ok)
	require.Equal(t, netip.MustParseAddrPort("10.0.0.1:6881"), addr)
}

func TestDHT(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	start := func() *dht.Server {
		s, err := dht.New(dht.Config{Addr: "127.0.0.1:0", QueryTimeout: 500 * time.Millisecond})
		require.NoError(t, err)
		go s.Run(ctx)
		return s
	}
	router, seeder, leecher := start(), start(), start()
	routers := []string{router.Addr().String()}
	cfg := DHTConfig{Routers: routers, MinNodes: 1, Interval: time.Minute, RetryInterval: 50 * time.Millisecond}

	infoHash := id.Random()
	go DHT(ctx, discard, seeder, swarm.New(swarm.Config{}), infoHash, 7000, cfg)

	// The leecher finds the seeder's announced port, retrying until the
	// announce has landed.
	sw := swarm.New(swarm.Config{})
	go DHT(ctx, discard, leecher, sw, infoHash, 7001, cfg)
	require.Eventually(t, func() bool {
		for {
			addr, ok := sw.Next()
			if !ok {
				return false
			}
			if addr.Port() == 7000 {
				return true
			}
		}
	}, 10*time.Second, 20*time.Millisecond)
}

func TestTrackerLoop(t *testing.T) {
	type call struct {
		event, left string
	}
	calls := make(chan call, 20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		calls <- call{q.Get("event"), q.Get("left")}
		w.Write([]byte(bencoding.EncodeDictionary(map[string]string{
			"interval": bencoding.EncodeInteger(1),
			"peers":    bencoding.EncodeString(""),
		})))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	var left atomic.Int64
	left.Store(1000)
	complete := make(chan struct{})
	done := make(chan struct{})
	go func() {
		TrackerLoop(ctx, discard, swarm.New(swarm.Config{}), srv.URL+"/announce", nil,
			tracker.Request{InfoHash: id.Random(), PeerID: id.NewPeerID()},
			func() Progress { return Progress{Left: int(left.Load())} },
			complete, TrackerLoopConfig{MinInterval: 10 * time.Millisecond})
		close(done)
	}()

	require.Equal(t, call{"started", "1000"}, <-calls)
	require.Equal(t, call{"", "1000"}, <-calls, "re-announces on the interval")

	left.Store(0)
	close(complete)
	for c := range calls {
		if c.event == "completed" {
			require.Equal(t, "0", c.left)
			break
		}
	}

	cancel()
	<-done
	var last call
	for len(calls) > 0 {
		last = <-calls
	}
	require.Equal(t, "stopped", last.event, "says goodbye on the way out")
}

func TestKeepBootstrapped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	start := func() *dht.Server {
		s, err := dht.New(dht.Config{Addr: "127.0.0.1:0", QueryTimeout: 200 * time.Millisecond})
		require.NoError(t, err)
		go s.Run(ctx)
		return s
	}
	router, node := start(), start()

	// No torrents at all: the node still joins.
	go KeepBootstrapped(ctx, discard, node, DHTConfig{
		Routers:       []string{router.Addr().String()},
		MinNodes:      1,
		Interval:      time.Minute,
		RetryInterval: 50 * time.Millisecond,
	})
	require.Eventually(t, func() bool { return node.Nodes() >= 1 }, 5*time.Second, 10*time.Millisecond)
}
