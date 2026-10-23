package daemon

import (
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/cnnrznn/torrent-ai/pkg/dht"
	"github.com/cnnrznn/torrent-ai/pkg/id"
	"github.com/cnnrznn/torrent-ai/pkg/swarm"
)

func TestStore(t *testing.T) {
	s, err := OpenStore(t.TempDir())
	require.NoError(t, err)

	h := id.Random()
	r := Record{InfoHash: h.Hex(), Magnet: "magnet:?xt=urn:btih:" + h.Hex(), Dir: "/data", Peers: []string{"10.0.0.1:6881"}, AddedAt: time.Now().UTC().Round(time.Second)}
	require.NoError(t, s.Save(r))
	require.NoError(t, s.SaveInfo(h, "d4:infoe"))

	records, err := s.Records()
	require.NoError(t, err)
	require.Equal(t, []Record{r}, records)

	raw, ok, err := s.LoadInfo(h)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "d4:infoe", raw)

	require.NoError(t, s.Delete(h))
	records, err = s.Records()
	require.NoError(t, err)
	require.Empty(t, records)
	_, ok, err = s.LoadInfo(h)
	require.NoError(t, err)
	require.False(t, ok)
	require.NoError(t, s.Delete(h), "deleting twice is fine")

	_, err = os.Stat(filepath.Join(s.Dir(), "torrents"))
	require.NoError(t, err)
	entries, _ := os.ReadDir(filepath.Join(s.Dir(), "torrents"))
	require.Empty(t, entries, "no temporary files left behind")
}

// TestRestore restarts a daemon: a magnet torrent comes back from the
// store using its cached info dict, so it skips the metadata fetch, and
// downloads only the pieces it's missing.
func TestRestore(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := makeTorrent(t, "a.bin", 400_000, 16*1024, 5)
	seeder := startNode(t, ctx, nil)
	seedDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(seedDir, "a.bin"), a.data, 0o644))
	_, err := seeder.m.Add(Source{Torrent: a.file, Dir: seedDir})
	require.NoError(t, err)
	waitState(t, seeder.m, a, StateSeeding)
	seedAddr := seeder.addr.AddrPort()

	stateDir, dir := t.TempDir(), t.TempDir()
	newManager := func(ctx context.Context) (*Manager, chan struct{}) {
		store, err := OpenStore(stateDir)
		require.NoError(t, err)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		m := New(Config{Listener: ln, Store: store, Swarm: swarm.Config{MaxPeers: 5}})
		done := make(chan struct{})
		go func() { m.Run(ctx); close(done) }()
		return m, done
	}

	// First run: fetch the metadata and the data from the magnet link.
	ctx1, stop1 := context.WithCancel(ctx)
	m1, done1 := newManager(ctx1)
	_, err = m1.Add(Source{Magnet: a.magnet(), Dir: dir, Peers: []netip.AddrPort{seedAddr}})
	require.NoError(t, err)
	waitState(t, m1, a, StateSeeding)
	stop1()
	<-done1

	// Damage the second half, as if the download had been interrupted.
	path := filepath.Join(dir, "a.bin")
	partial := append([]byte(nil), a.data...)
	for i := len(partial) / 2; i < len(partial); i++ {
		partial[i] = 0
	}
	require.NoError(t, os.WriteFile(path, partial, 0o644))

	// Second run: restored from the store.
	m2, done2 := newManager(ctx)
	defer func() { cancel(); <-done2 }()
	n, err := m2.Restore()
	require.NoError(t, err)
	require.Equal(t, 1, n)
	st, err := m2.Status(a.meta.InfoHash())
	require.NoError(t, err)
	require.Contains(t, []State{StateVerifying, StateDownloading}, st.State, "the cached info dict skips the metadata fetch")
	require.Equal(t, "a.bin", st.Name)

	st = waitState(t, m2, a, StateSeeding)
	require.Less(t, st.Downloaded, int64(len(a.data)), "only the damaged pieces are downloaded again")
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, a.data, got)

	// Removing forgets it.
	require.NoError(t, m2.Remove(a.meta.InfoHash(), false))
	store, _ := OpenStore(stateDir)
	records, err := store.Records()
	require.NoError(t, err)
	require.Empty(t, records)
}

func TestStoreDHT(t *testing.T) {
	s, err := OpenStore(t.TempDir())
	require.NoError(t, err)
	_, ok, err := s.LoadDHT()
	require.NoError(t, err)
	require.False(t, ok)

	want := DHTState{ID: id.Random().Hex(), Nodes: []dht.NodeInfo{{ID: id.Random().Hex(), Addr: "1.2.3.4:6881"}}, SavedAt: time.Now().UTC().Round(time.Second)}
	require.NoError(t, s.SaveDHT(want))
	got, ok, err := s.LoadDHT()
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, want, got)
}
