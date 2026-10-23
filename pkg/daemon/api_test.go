package daemon

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAPI(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := startNode(t, ctx, nil)

	dir := t.TempDir()
	a := makeTorrent(t, "a.bin", 60_000, 16*1024, 7)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.bin"), a.data, 0o644))

	var ready atomic.Bool
	ready.Store(true)
	srv := httptest.NewServer(NewAPI(n.m, APIConfig{
		Token:      "s3cret",
		DefaultDir: dir,
		Ready:      func() (bool, string) { return ready.Load(), "dht has 0 nodes" },
	}))
	defer srv.Close()

	c, err := NewClient(ClientConfig{Addr: srv.URL, Token: "s3cret"})
	require.NoError(t, err)

	t.Run("adds a torrent in the default dir", func(t *testing.T) {
		st, err := c.Add(ctx, AddRequest{Torrent: a.file})
		require.NoError(t, err)
		require.Equal(t, a.meta.InfoHash().Hex(), st.InfoHash)
		require.Equal(t, dir, st.Dir)
		waitState(t, n.m, a, StateSeeding)
	})

	t.Run("lists and gets", func(t *testing.T) {
		list, err := c.List(ctx)
		require.NoError(t, err)
		require.Len(t, list, 1)
		st, err := c.Status(ctx, a.meta.InfoHash().Hex())
		require.NoError(t, err)
		require.Equal(t, StateSeeding, st.State)
		require.Zero(t, st.Left)
	})

	t.Run("reports errors with status codes", func(t *testing.T) {
		var re *RemoteError
		_, err := c.Add(ctx, AddRequest{Torrent: a.file})
		require.True(t, errors.As(err, &re))
		require.Equal(t, http.StatusConflict, re.Status)
		require.Equal(t, a.meta.InfoHash().Hex(), re.InfoHash)

		_, err = c.Add(ctx, AddRequest{Magnet: "magnet:?xt=urn:btih:bad"})
		require.True(t, errors.As(err, &re))
		require.Equal(t, http.StatusBadRequest, re.Status)

		_, err = c.Add(ctx, AddRequest{Torrent: a.file, Dir: "relative/path"})
		require.ErrorContains(t, err, "absolute")

		_, err = c.Status(ctx, "00")
		require.True(t, errors.As(err, &re))
		require.Equal(t, http.StatusBadRequest, re.Status)

		_, err = c.Status(ctx, "0000000000000000000000000000000000000000")
		require.True(t, errors.As(err, &re))
		require.Equal(t, http.StatusNotFound, re.Status)
	})

	t.Run("requires the token on /v1 but not on health checks", func(t *testing.T) {
		anon, _ := NewClient(ClientConfig{Addr: srv.URL})
		_, err := anon.List(ctx)
		var re *RemoteError
		require.True(t, errors.As(err, &re))
		require.Equal(t, http.StatusUnauthorized, re.Status)

		res, err := http.Get(srv.URL + "/healthz")
		require.NoError(t, err)
		res.Body.Close()
		require.Equal(t, http.StatusOK, res.StatusCode)
	})

	t.Run("readyz follows the ready check", func(t *testing.T) {
		res, err := http.Get(srv.URL + "/readyz")
		require.NoError(t, err)
		res.Body.Close()
		require.Equal(t, http.StatusOK, res.StatusCode)

		ready.Store(false)
		res, err = http.Get(srv.URL + "/readyz")
		require.NoError(t, err)
		res.Body.Close()
		require.Equal(t, http.StatusServiceUnavailable, res.StatusCode)
		ready.Store(true)
	})

	t.Run("removes", func(t *testing.T) {
		require.NoError(t, c.Remove(ctx, a.meta.InfoHash().Hex(), false))
		list, err := c.List(ctx)
		require.NoError(t, err)
		require.Empty(t, list)
	})
}

func TestClientOverUnixSocket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := startNode(t, ctx, nil)

	// Unix socket paths have a short length limit, so use a short dir.
	sockDir, err := os.MkdirTemp("", "tai")
	require.NoError(t, err)
	defer os.RemoveAll(sockDir)
	sock := filepath.Join(sockDir, "api.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	srv := &http.Server{Handler: NewAPI(n.m, APIConfig{DefaultDir: t.TempDir()}), ReadHeaderTimeout: time.Second}
	go srv.Serve(ln)
	defer srv.Close()

	c, err := NewClient(ClientConfig{Socket: sock})
	require.NoError(t, err)
	list, err := c.List(ctx)
	require.NoError(t, err)
	require.Empty(t, list)
}
