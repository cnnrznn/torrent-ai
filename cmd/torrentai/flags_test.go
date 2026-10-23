package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/pprof"
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

func TestRegistryEnv(t *testing.T) {
	newEnvRegistry := func(env map[string]string) (*registry, *config) {
		cfg := defaultConfig()
		r := newRegistry("torrent", "<target>")
		registerFlags(r, &cfg)
		r.fs.SetOutput(io.Discard)
		r.lookup = func(k string) (string, bool) { v, ok := env[k]; return v, ok }
		return r, &cfg
	}

	t.Run("environment variables set flags", func(t *testing.T) {
		r, cfg := newEnvRegistry(map[string]string{
			"TORRENT_AI_MAX_PEERS":   "20",
			"TORRENT_AI_DHT":         "false",
			"TORRENT_AI_MAX_MEMORY":  "64MiB",
			"TORRENT_AI_DHT_ROUTERS": "127.0.0.1:7000,127.0.0.1:7001",
		})
		require.NoError(t, r.parse(nil))
		require.Equal(t, 20, cfg.swarm.MaxPeers)
		require.False(t, cfg.useDHT)
		require.EqualValues(t, 64<<20, cfg.session.MaxMemory)
		require.Equal(t, []string{"127.0.0.1:7000", "127.0.0.1:7001"}, cfg.dhtRouters)
		require.ElementsMatch(t, []string{"max-peers", "dht", "max-memory", "dht-routers"}, r.fromEnv)
	})

	t.Run("a switch is turned on by being present", func(t *testing.T) {
		r, cfg := newEnvRegistry(map[string]string{"TORRENT_AI_SEED": ""})
		require.NoError(t, r.parse(nil))
		require.True(t, cfg.seed)
	})

	t.Run("the command line wins", func(t *testing.T) {
		r, cfg := newEnvRegistry(map[string]string{"TORRENT_AI_MAX_PEERS": "20"})
		require.NoError(t, r.parse([]string{"-max-peers", "5"}))
		require.Equal(t, 5, cfg.swarm.MaxPeers)
	})

	t.Run("bad values name the variable", func(t *testing.T) {
		r, _ := newEnvRegistry(map[string]string{"TORRENT_AI_BLOCK_SIZE": "lots"})
		require.ErrorContains(t, r.parse(nil), "TORRENT_AI_BLOCK_SIZE")
	})

	t.Run("environment values are range-checked", func(t *testing.T) {
		r, _ := newEnvRegistry(map[string]string{"TORRENT_AI_DHT_K": "0"})
		require.ErrorContains(t, r.parse(nil), "-dht-k must be at least 1")
	})

	require.Equal(t, "TORRENT_AI_UDP_CONNECTION_ID_LIFETIME", envName("udp-connection-id-lifetime"))
	require.Equal(t, "TORRENT_AI_O", envName("o"))
}

func TestFlameSwitch(t *testing.T) {
	parse := func(env map[string]string, args ...string) (config, error) {
		cfg := defaultConfig()
		r := newRegistry("torrent", "<target>")
		registerFlags(r, &cfg)
		r.fs.SetOutput(io.Discard)
		r.lookup = func(k string) (string, bool) { v, ok := env[k]; return v, ok }
		if err := r.parse(args); err != nil {
			return cfg, err
		}
		return cfg, cfg.validate()
	}

	for _, tc := range []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{"off by default", nil, nil, ""},
		{"present but empty", map[string]string{"TORRENT_AI_FLAME": ""}, nil, defaultFlameAddr},
		{"set to 1", map[string]string{"TORRENT_AI_FLAME": "1"}, nil, defaultFlameAddr},
		{"set to an address", map[string]string{"TORRENT_AI_FLAME": ":7070"}, nil, ":7070"},
		{"turned off", map[string]string{"TORRENT_AI_FLAME": "false"}, nil, ""},
		{"bare flag", nil, []string{"-flame"}, defaultFlameAddr},
		{"flag with address", nil, []string{"-flame=127.0.0.1:7071"}, "127.0.0.1:7071"},
		{"flag overrides env", map[string]string{"TORRENT_AI_FLAME": ""}, []string{"-flame=false"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := parse(tc.env, tc.args...)
			require.NoError(t, err)
			require.Equal(t, tc.want, cfg.profiling.addr)
		})
	}

	_, err := parse(map[string]string{"TORRENT_AI_FLAME": "not an address"})
	require.ErrorContains(t, err, "-flame")
}

func TestFlameEndpoint(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cpu := filepath.Join(t.TempDir(), "cpu.pprof")
	heap := filepath.Join(t.TempDir(), "heap.pprof")
	p := profiling{addr: "127.0.0.1:0", cpuFile: cpu, heapFile: heap}

	// Listen on a known free port so the test can reach it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	p.addr = ln.Addr().String()
	ln.Close()

	stop, err := p.start(ctx, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	res, err := http.Get("http://" + p.addr + "/debug/pprof/goroutine?debug=1")
	require.NoError(t, err)
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Contains(t, string(body), "goroutine profile")

	stop()
	for _, f := range []string{cpu, heap} {
		st, err := os.Stat(f)
		require.NoError(t, err)
		require.Positive(t, st.Size(), f)
	}
}

func TestLabeled(t *testing.T) {
	labeled(context.Background(), "dht", func(ctx context.Context) {
		v, ok := pprof.Label(ctx, "component")
		require.True(t, ok)
		require.Equal(t, "dht", v)
	})
}
