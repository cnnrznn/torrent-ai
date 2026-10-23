package torrent

import (
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/cnnrznn/torrent-scratch/pkg/id"
	"github.com/cnnrznn/torrent-scratch/pkg/metainfo"
	"github.com/cnnrznn/torrent-scratch/pkg/swarm"
)

// writeContent lays the torrent's content out on disk under dir.
func writeContent(t *testing.T, dir string, m *metainfo.Metainfo, data []byte) {
	t.Helper()
	for _, f := range m.Files() {
		path := filepath.Join(append([]string{dir}, f.Path...)...)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, data[:f.Length], 0o644))
		data = data[f.Length:]
	}
}

// readContent reads the torrent's content back from disk.
func readContent(t *testing.T, dir string, m *metainfo.Metainfo) []byte {
	t.Helper()
	var data []byte
	for _, f := range m.Files() {
		b, err := os.ReadFile(filepath.Join(append([]string{dir}, f.Path...)...))
		require.NoError(t, err)
		data = append(data, b...)
	}
	return data
}

// startSession runs a session listening on localhost and returns its
// address.
func startSession(t *testing.T, ctx context.Context, m *metainfo.Metainfo, dir string, peers ...netip.AddrPort) (*Session, netip.AddrPort) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	sw := swarm.New(10)
	sw.Add(peers...)
	s, err := New(m, sw, Config{Dir: dir, PeerID: id.NewPeerID(), Listener: ln})
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() { <-done })

	return s, ln.Addr().(*net.TCPAddr).AddrPort()
}

func waitComplete(t *testing.T, s *Session) {
	t.Helper()
	select {
	case <-s.Complete():
	case <-time.After(20 * time.Second):
		t.Fatalf("download did not complete: %+v", s.Stats())
	}
}

func TestSessionDownload(t *testing.T) {
	m, data := makeTorrent(t, 32*1024, 300_000, 1, 123_457)

	t.Run("a seeder verifies its data on startup", func(t *testing.T) {
		dir := t.TempDir()
		writeContent(t, dir, m, data)
		s, err := New(m, swarm.New(1), Config{Dir: dir})
		require.NoError(t, err)
		defer s.storage.Close()

		waitComplete(t, s)
		require.Zero(t, s.Stats().Left)
	})

	t.Run("downloads from a seeder", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		seedDir := t.TempDir()
		writeContent(t, seedDir, m, data)
		_, seedAddr := startSession(t, ctx, m, seedDir)

		dir := t.TempDir()
		leech, _ := startSession(t, ctx, m, dir, seedAddr)
		waitComplete(t, leech)
		require.Equal(t, data, readContent(t, dir, m))
	})

	t.Run("leechers share pieces with each other", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		seedDir := t.TempDir()
		writeContent(t, seedDir, m, data)
		_, seedAddr := startSession(t, ctx, m, seedDir)

		dirA, dirB := t.TempDir(), t.TempDir()
		a, addrA := startSession(t, ctx, m, dirA, seedAddr)
		b, _ := startSession(t, ctx, m, dirB, seedAddr, addrA)

		waitComplete(t, a)
		waitComplete(t, b)
		require.Equal(t, data, readContent(t, dirA, m))
		require.Equal(t, data, readContent(t, dirB, m))
	})

	t.Run("resumes a partial download", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		seedDir := t.TempDir()
		writeContent(t, seedDir, m, data)
		_, seedAddr := startSession(t, ctx, m, seedDir)

		// Start with the first half intact and the rest corrupted.
		dir := t.TempDir()
		partial := append([]byte(nil), data...)
		for i := len(partial) / 2; i < len(partial); i++ {
			partial[i] ^= 0xff
		}
		writeContent(t, dir, m, partial)

		leech, _ := startSession(t, ctx, m, dir, seedAddr)
		have := leech.Stats().Have
		require.Greater(t, have, 0)
		require.Less(t, have, m.NumPieces())

		waitComplete(t, leech)
		require.Equal(t, data, readContent(t, dir, m))
		require.Less(t, leech.Stats().Downloaded, int64(len(data)))
	})
}
