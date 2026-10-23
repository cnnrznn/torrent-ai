package daemon

import (
	"bytes"
	"context"
	"crypto/sha1"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cnnrznn/torrent-ai/pkg/bencoding"
	"github.com/cnnrznn/torrent-ai/pkg/dht"
	"github.com/cnnrznn/torrent-ai/pkg/metainfo"
	"github.com/cnnrznn/torrent-ai/pkg/peersource"
	"github.com/cnnrznn/torrent-ai/pkg/swarm"
)

// testTorrent is a single-file torrent over random content.
type testTorrent struct {
	file []byte // the .torrent
	meta *metainfo.Metainfo
	data []byte
}

func makeTorrent(t *testing.T, name string, size, pieceLength int, seed uint64) testTorrent {
	t.Helper()
	r := rand.New(rand.NewPCG(seed, seed))
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(r.UintN(256))
	}
	var pieces []byte
	for off := 0; off < size; off += pieceLength {
		h := sha1.Sum(data[off:min(off+pieceLength, size)])
		pieces = append(pieces, h[:]...)
	}
	info := bencoding.EncodeDictionary(map[string]string{
		"name":         bencoding.EncodeString(name),
		"length":       bencoding.EncodeInteger(size),
		"piece length": bencoding.EncodeInteger(pieceLength),
		"pieces":       bencoding.EncodeString(string(pieces)),
	})
	file := bencoding.EncodeDictionary(map[string]string{"info": info})
	meta, err := metainfo.New(file)
	require.NoError(t, err)
	return testTorrent{file: []byte(file), meta: meta, data: data}
}

func (tt testTorrent) magnet() string {
	return "magnet:?xt=urn:btih:" + tt.meta.InfoHash().Hex()
}

// node is a Manager with its own listener and DHT node.
type node struct {
	m    *Manager
	dht  *dht.Server
	addr *net.TCPAddr
}

func startNode(t *testing.T, ctx context.Context, routers []string) *node {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv, err := dht.New(dht.Config{Addr: "127.0.0.1:0", QueryTimeout: 500 * time.Millisecond})
	require.NoError(t, err)
	go srv.Run(ctx)

	port := ln.Addr().(*net.TCPAddr).Port
	m := New(Config{
		Listener: ln,
		Port:     port,
		DHT:      srv,
		DHTSource: peersource.DHTConfig{
			Routers:       routers,
			MinNodes:      1,
			Interval:      time.Minute,
			RetryInterval: 100 * time.Millisecond,
		},
		MaxPeers:  20,
		MaxMemory: 1 << 20,
		Swarm:     swarm.Config{MaxPeers: 10},
	})
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() { <-done })
	return &node{m: m, dht: srv, addr: ln.Addr().(*net.TCPAddr)}
}

func waitState(t *testing.T, m *Manager, tt testTorrent, want State) Status {
	t.Helper()
	var st Status
	var err error
	ok := assert.Eventually(t, func() bool {
		st, err = m.Status(tt.meta.InfoHash())
		return err == nil && st.State == want
	}, 20*time.Second, 20*time.Millisecond)
	require.True(t, ok, "waiting for %s: last status %+v, err %v", want, st, err)
	return st
}

// TestTwoTorrentsOneProcess is the phase 1 milestone: two torrents
// served through one listener and one DHT node, found and downloaded
// from bare magnet links by another daemon.
func TestTwoTorrentsOneProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := makeTorrent(t, "a.bin", 300_000, 32*1024, 1)
	b := makeTorrent(t, "b.bin", 120_000, 16*1024, 2)

	// A bare DHT node both daemons bootstrap from.
	router, err := dht.New(dht.Config{Addr: "127.0.0.1:0"})
	require.NoError(t, err)
	go router.Run(ctx)
	routers := []string{router.Addr().String()}

	seeder := startNode(t, ctx, routers)
	seedDir := t.TempDir()
	for _, tt := range []testTorrent{a, b} {
		require.NoError(t, os.WriteFile(filepath.Join(seedDir, tt.meta.Name()), tt.data, 0o644))
		_, err := seeder.m.Add(Source{Torrent: tt.file, Dir: seedDir})
		require.NoError(t, err)
		waitState(t, seeder.m, tt, StateSeeding)
	}

	leecher := startNode(t, ctx, routers)
	dir := t.TempDir()
	for _, tt := range []testTorrent{a, b} {
		_, err := leecher.m.Add(Source{Magnet: tt.magnet(), Dir: dir})
		require.NoError(t, err)
	}
	for _, tt := range []testTorrent{a, b} {
		st := waitState(t, leecher.m, tt, StateSeeding)
		require.Equal(t, tt.meta.Name(), st.Name)
		require.Zero(t, st.Left)
		got, err := os.ReadFile(filepath.Join(dir, tt.meta.Name()))
		require.NoError(t, err)
		require.Equal(t, tt.data, got)
	}

	list := leecher.m.List()
	require.Len(t, list, 2)
	require.Equal(t, a.meta.InfoHash().Hex(), list[0].InfoHash, "oldest first")
}

func TestManagerAddAndRemove(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := startNode(t, ctx, nil)
	a := makeTorrent(t, "a.bin", 50_000, 16*1024, 3)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.bin"), a.data, 0o644))

	t.Run("rejects bad sources", func(t *testing.T) {
		_, err := n.m.Add(Source{Dir: dir})
		require.ErrorIs(t, err, ErrNoSource)
		_, err = n.m.Add(Source{Magnet: "magnet:?xt=urn:btih:nope", Dir: dir})
		require.Error(t, err)
		_, err = n.m.Add(Source{Torrent: a.file})
		require.ErrorContains(t, err, "dir")
	})

	t.Run("rejects duplicates", func(t *testing.T) {
		_, err := n.m.Add(Source{Torrent: a.file, Dir: dir})
		require.NoError(t, err)
		_, err = n.m.Add(Source{Magnet: a.magnet(), Dir: dir})
		require.ErrorIs(t, err, ErrExists)
		waitState(t, n.m, a, StateSeeding)
	})

	t.Run("removes, optionally deleting files", func(t *testing.T) {
		require.NoError(t, n.m.Remove(a.meta.InfoHash(), false))
		require.FileExists(t, filepath.Join(dir, "a.bin"))
		_, err := n.m.Status(a.meta.InfoHash())
		require.ErrorIs(t, err, ErrNotFound)
		require.ErrorIs(t, n.m.Remove(a.meta.InfoHash(), false), ErrNotFound)

		_, err = n.m.Add(Source{Torrent: a.file, Dir: dir})
		require.NoError(t, err)
		waitState(t, n.m, a, StateSeeding)
		require.NoError(t, n.m.Remove(a.meta.InfoHash(), true))
		require.NoFileExists(t, filepath.Join(dir, "a.bin"))
	})

	t.Run("removing during the metadata fetch stops it", func(t *testing.T) {
		b := makeTorrent(t, "b.bin", 10_000, 16*1024, 4)
		_, err := n.m.Add(Source{Magnet: b.magnet(), Dir: dir})
		require.NoError(t, err)
		st, err := n.m.Status(b.meta.InfoHash())
		require.NoError(t, err)
		require.Equal(t, StateMetadata, st.State)
		require.NoError(t, n.m.Remove(b.meta.InfoHash(), true))
	})
}

func TestTrackerEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan string, 20)
	var lefts sync.Map
	tr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		events <- q.Get("event")
		lefts.Store(q.Get("event"), q.Get("left"))
		w.Write([]byte(bencoding.EncodeDictionary(map[string]string{
			"interval": bencoding.EncodeInteger(3600),
			"peers":    bencoding.EncodeString(""),
		})))
	}))
	defer tr.Close()

	n := startNode(t, ctx, nil)
	a := makeTorrent(t, "a.bin", 40_000, 16*1024, 11)
	seedDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(seedDir, "a.bin"), a.data, 0o644))
	seeder := startNode(t, ctx, nil)
	_, err := seeder.m.Add(Source{Torrent: a.file, Dir: seedDir})
	require.NoError(t, err)
	waitState(t, seeder.m, a, StateSeeding)

	_, err = n.m.Add(Source{Magnet: a.magnet(), Dir: t.TempDir(), Trackers: []string{tr.URL + "/announce"}, Peers: []netip.AddrPort{seeder.addr.AddrPort()}})
	require.NoError(t, err)
	waitState(t, n.m, a, StateSeeding)

	next := func() string {
		select {
		case e := <-events:
			return e
		case <-time.After(10 * time.Second):
			t.Fatal("no announce")
			return ""
		}
	}
	require.Equal(t, "started", next())
	require.Equal(t, "completed", next())
	left, _ := lefts.Load("completed")
	require.Equal(t, "0", left)

	require.NoError(t, n.m.Remove(a.meta.InfoHash(), false))
	require.Equal(t, "stopped", next())
}

func TestProfileLabels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := New(Config{})
	go m.Run(ctx)

	a := makeTorrent(t, "a.bin", 20_000, 16*1024, 17)
	_, err := m.Add(Source{Magnet: a.magnet(), Dir: t.TempDir()})
	require.NoError(t, err)

	// The torrent's goroutines carry its labels, even though Add ran
	// on an unlabeled goroutine.
	require.Eventually(t, func() bool {
		var buf bytes.Buffer
		pprof.Lookup("goroutine").WriteTo(&buf, 1)
		out := buf.String()
		return strings.Contains(out, `"torrent":"`+a.meta.InfoHash().Hex()+`"`) &&
			strings.Contains(out, `"component":"metadata"`)
	}, 5*time.Second, 20*time.Millisecond)
}
