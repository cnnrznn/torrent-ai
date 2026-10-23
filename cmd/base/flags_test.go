package main

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/cnnrznn/torrent-ai/pkg/dht"
	"github.com/cnnrznn/torrent-ai/pkg/peerwire"
	"github.com/cnnrznn/torrent-ai/pkg/torrent"
)

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{
		"4096":   4096,
		"16KiB":  16 << 10,
		"16k":    16 << 10,
		"2MiB":   2 << 20,
		"2MB":    2 << 20,
		"1G":     1 << 30,
		"0":      0,
		"10 KiB": 10 << 10,
	} {
		got, err := parseSize(in)
		require.NoError(t, err, in)
		require.Equal(t, want, got, in)
	}
	for _, in := range []string{"", "KiB", "1.5M", "12X"} {
		_, err := parseSize(in)
		require.Error(t, err, in)
	}

	require.Equal(t, "16KiB", formatSize(16<<10))
	require.Equal(t, "2MiB", formatSize(2<<20))
	require.Equal(t, "1000", formatSize(1000))
	require.Equal(t, "0", formatSize(0))
}

func TestRegistry(t *testing.T) {
	newTestRegistry := func() (*registry, *int, *time.Duration, *[]string) {
		r := newRegistry("torrent", "<target>")
		n, d := 8, 5*time.Second
		var l []string
		r.group("Things")
		r.integer(&n, "k", 1, "bucket size", "BEP 5")
		r.duration(&d, "wait", 0, "how long to wait", "")
		r.list(&l, "peer", "peers to add")
		return r, &n, &d, &l
	}

	t.Run("help groups flags and marks spec-defined defaults", func(t *testing.T) {
		r, _, _, _ := newTestRegistry()
		var buf bytes.Buffer
		r.printUsage(&buf)
		out := buf.String()
		require.Contains(t, out, "Things:")
		require.Contains(t, out, "-k int")
		require.Contains(t, out, "bucket size (default 8) [spec-defined recommended default: BEP 5]")
		require.Contains(t, out, "how long to wait (default 5s)\n")
		require.Contains(t, out, "Exit codes:")
	})

	t.Run("rejects values below the minimum", func(t *testing.T) {
		r, _, _, _ := newTestRegistry()
		require.ErrorContains(t, r.parse([]string{"-k", "0"}), "-k must be at least 1")
	})

	t.Run("reports overridden spec defaults", func(t *testing.T) {
		r, n, _, _ := newTestRegistry()
		require.NoError(t, r.parse([]string{"-k", "20", "-wait", "1s"}))
		require.Equal(t, 20, *n)
		require.Equal(t, []specOverride{{name: "k", value: "20", def: "8", spec: "BEP 5"}}, r.specOverrides())
	})

	t.Run("setting a spec flag to its default is not an override", func(t *testing.T) {
		r, _, _, _ := newTestRegistry()
		require.NoError(t, r.parse([]string{"-k", "8"}))
		require.Empty(t, r.specOverrides())
	})

	t.Run("lists accept repeats and commas", func(t *testing.T) {
		r, _, _, l := newTestRegistry()
		require.NoError(t, r.parse([]string{"-peer", "a:1,b:2", "-peer", "c:3"}))
		require.Equal(t, []string{"a:1", "b:2", "c:3"}, *l)
	})

	t.Run("values lists every flag in help order", func(t *testing.T) {
		r, _, _, _ := newTestRegistry()
		require.NoError(t, r.parse(nil))
		require.Equal(t, []any{"k", "8", "wait", "5s", "peer", "[]"}, r.values())
	})
}

func TestConfigFlags(t *testing.T) {
	parse := func(args ...string) (config, error) {
		cfg := defaultConfig()
		r := newRegistry("torrent", "<target>")
		registerFlags(r, &cfg)
		r.fs.SetOutput(io.Discard)
		if err := r.parse(args); err != nil {
			return cfg, err
		}
		cfg.finish()
		return cfg, cfg.validate()
	}

	t.Run("defaults come from the packages", func(t *testing.T) {
		cfg, err := parse()
		require.NoError(t, err)
		require.Equal(t, dht.DefaultConfig().K, cfg.dht.K)
		require.Equal(t, torrent.DefaultConfig().BlockSize, cfg.session.BlockSize)
		require.Equal(t, peerwire.DefaultOptions(), cfg.session.Conn)
		require.Equal(t, cfg.session.Conn, cfg.metadata.Conn)
		require.Equal(t, dht.DefaultRouters, cfg.dhtRouters)
	})

	t.Run("flags reach the package configs", func(t *testing.T) {
		cfg, err := parse("-max-memory", "64MiB", "-keepalive", "30s", "-metadata-piece-size", "8KiB", "-session-tick", "250ms", "-dht-alpha", "5", "-max-peers", "7", "-o", "/tmp/x")
		require.NoError(t, err)
		require.EqualValues(t, 64<<20, cfg.session.MaxMemory)
		require.Equal(t, 30*time.Second, cfg.metadata.Conn.KeepAlive)
		require.Equal(t, 8<<10, cfg.metadata.Metadata.PieceSize)
		require.Equal(t, 250*time.Millisecond, cfg.metadata.Tick)
		require.Equal(t, 5, cfg.dht.Alpha)
		require.Equal(t, 7, cfg.swarm.MaxPeers)
		require.Equal(t, "/tmp/x", cfg.session.Dir)
	})

	t.Run("dht-routers replaces the defaults", func(t *testing.T) {
		cfg, err := parse("-dht-routers", "127.0.0.1:7000")
		require.NoError(t, err)
		require.Equal(t, []string{"127.0.0.1:7000"}, cfg.dhtRouters)
	})

	t.Run("rejects malformed addresses and URLs", func(t *testing.T) {
		_, err := parse("-peer", "no-port")
		require.ErrorContains(t, err, "-peer")
		_, err = parse("-dht-routers", "no-port")
		require.ErrorContains(t, err, "-dht-routers")
		_, err = parse("-tracker", "ftp://x/announce")
		require.ErrorContains(t, err, "-tracker")
		_, err = parse("-log-format", "xml")
		require.ErrorContains(t, err, "-log-format")
	})
}

func TestNextLookup(t *testing.T) {
	cfg := defaultConfig()
	cfg.dhtRetryInterval = 5 * time.Second
	cfg.dhtInterval = 30 * time.Second

	// Unhealthy lookups retry sooner, backing off up to the interval.
	retry := cfg.dhtRetryInterval
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
