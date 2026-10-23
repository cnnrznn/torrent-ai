package torrent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStorage(t *testing.T) {
	m, data := makeTorrent(t, 16, 10, 0, 25, 5)
	dir := t.TempDir()

	s, existed, err := OpenStorage(dir, m)
	require.NoError(t, err)
	require.False(t, existed)

	t.Run("writes across file boundaries", func(t *testing.T) {
		require.NoError(t, s.WriteAt(data[:20], 0))
		require.NoError(t, s.WriteAt(data[20:], 20))

		a, _ := os.ReadFile(filepath.Join(dir, "test", "f", "a"))
		c, _ := os.ReadFile(filepath.Join(dir, "test", "f", "c"))
		d, _ := os.ReadFile(filepath.Join(dir, "test", "f", "d"))
		require.Equal(t, data[:10], a)
		require.Equal(t, data[10:35], c)
		require.Equal(t, data[35:], d)
		require.FileExists(t, filepath.Join(dir, "test", "f", "b"))
	})

	t.Run("reads across file boundaries", func(t *testing.T) {
		got := make([]byte, 30)
		require.NoError(t, s.ReadAt(got, 8))
		require.Equal(t, data[8:38], got)
	})

	t.Run("rejects out of range access", func(t *testing.T) {
		require.Error(t, s.WriteAt([]byte("xx"), int64(len(data)-1)))
		require.Error(t, s.ReadAt(make([]byte, 1), -1))
	})

	require.NoError(t, s.Close())

	t.Run("reports existing files", func(t *testing.T) {
		s, existed, err := OpenStorage(dir, m)
		require.NoError(t, err)
		defer s.Close()
		require.True(t, existed)

		got := make([]byte, len(data))
		require.NoError(t, s.ReadAt(got, 0))
		require.Equal(t, data, got)
	})
}
