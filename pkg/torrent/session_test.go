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

	"github.com/cnnrznn/torrent-ai/pkg/id"
	"github.com/cnnrznn/torrent-ai/pkg/metainfo"
	"github.com/cnnrznn/torrent-ai/pkg/peerwire"
	"github.com/cnnrznn/torrent-ai/pkg/swarm"
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

func TestSessionServesMetadata(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m, data := makeTorrent(t, 32*1024, 100_000)
	dir := t.TempDir()
	writeContent(t, dir, m, data)
	_, addr := startSession(t, ctx, m, dir)

	c, err := peerwire.Dial(ctx, addr, peerwire.NewHandshake(m.InfoHash(), id.NewPeerID(), false), peerwire.Options{})
	require.NoError(t, err)
	defer c.Close()

	info, err := peerwire.FetchMetadata(ctx, c, m.InfoHash(), peerwire.MetadataOptions{})
	require.NoError(t, err)
	require.Equal(t, m.RawInfo(), info)
}

func TestSessionDHTPort(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m, _ := makeTorrent(t, 32*1024, 100_000)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().(*net.TCPAddr).AddrPort()

	ports := make(chan netip.AddrPort, 1)
	s, err := New(m, swarm.New(10), Config{
		Dir:      t.TempDir(),
		PeerID:   id.NewPeerID(),
		Listener: ln,
		DHTPort:  6881,
		OnPort:   func(a netip.AddrPort) { ports <- a },
	})
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()
	defer func() { cancel(); <-done }()

	c, err := peerwire.Dial(ctx, addr, peerwire.NewHandshake(m.InfoHash(), id.NewPeerID(), true), peerwire.Options{})
	require.NoError(t, err)
	defer c.Close()
	require.True(t, c.Peer.SupportsDHT())

	t.Run("sends its DHT port", func(t *testing.T) {
		for msg := range c.Messages() {
			if msg.ID == peerwire.Port {
				port, err := msg.ParsePort()
				require.NoError(t, err)
				require.EqualValues(t, 6881, port)
				return
			}
		}
		t.Fatal("no port message")
	})

	t.Run("reports the peer's DHT port", func(t *testing.T) {
		require.NoError(t, c.Send(peerwire.MsgPort(7777)))
		select {
		case got := <-ports:
			require.Equal(t, netip.AddrPortFrom(addr.Addr(), 7777), got)
		case <-time.After(5 * time.Second):
			t.Fatal("OnPort not called")
		}
	})
}

func TestSessionRecoversFromBadData(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m, data := makeTorrent(t, 32*1024, 300_000)

	// The liar verifies good data on startup, then its file is
	// corrupted underneath it, so it serves blocks that fail the hash.
	liarDir := t.TempDir()
	writeContent(t, liarDir, m, data)
	_, liarAddr := startSession(t, ctx, m, liarDir)
	bad := append([]byte(nil), data...)
	for i := range bad {
		bad[i] ^= 0xff
	}
	writeContent(t, liarDir, m, bad)

	honestDir := t.TempDir()
	writeContent(t, honestDir, m, data)
	_, honestAddr := startSession(t, ctx, m, honestDir)

	dir := t.TempDir()
	leech, _ := startSession(t, ctx, m, dir, liarAddr, honestAddr)
	waitComplete(t, leech)

	require.Equal(t, data, readContent(t, dir, m))
	st := leech.Stats()
	require.Positive(t, st.HashFailures)
	require.LessOrEqual(t, st.HashFailures, int64(maxHashFailures))
	require.Less(t, st.Downloaded, 3*int64(len(data)), "bad data must not be re-fetched endlessly")
	require.Equal(t, 1, leech.swarm.Banned())
}
