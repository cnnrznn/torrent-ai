package torrent

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/cnnrznn/torrent-scratch/pkg/id"
	"github.com/cnnrznn/torrent-scratch/pkg/swarm"
)

func TestFetchMetadata(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	m, data := makeTorrent(t, 32*1024, 200_000, 50_000)
	seedDir := t.TempDir()
	writeContent(t, seedDir, m, data)
	_, seedAddr := startSession(t, ctx, m, seedDir)

	// An address nobody listens on, to exercise a failed attempt.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	dead := ln.Addr().(*net.TCPAddr).AddrPort()
	ln.Close()

	sw := swarm.New(10)
	sw.Add(dead)
	go func() {
		// The real peer turns up later, like a slow tracker reply.
		time.Sleep(100 * time.Millisecond)
		sw.Add(seedAddr)
	}()

	got, err := FetchMetadata(ctx, sw, m.InfoHash(), id.NewPeerID(), nil)
	require.NoError(t, err)
	require.Equal(t, m.RawInfo(), got.RawInfo())

	// The fetched metainfo is enough to download the torrent.
	dir := t.TempDir()
	leech, _ := startSession(t, ctx, got, dir, seedAddr)
	waitComplete(t, leech)
	require.Equal(t, data, readContent(t, dir, got))
}
