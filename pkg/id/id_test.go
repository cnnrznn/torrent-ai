package id

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFromString(t *testing.T) {
	t.Run("accepts 20 bytes", func(t *testing.T) {
		h, err := FromString(strings.Repeat("a", 20))
		require.NoError(t, err)
		require.Equal(t, strings.Repeat("a", 20), h.String())
	})

	t.Run("rejects other lengths", func(t *testing.T) {
		_, err := FromString("short")
		require.Error(t, err)
	})
}

func TestFromHex(t *testing.T) {
	const s = "c9e15763f722f23e98a29decdfae341b98d53056"
	h, err := FromHex(s)
	require.NoError(t, err)
	require.Equal(t, s, h.Hex())

	_, err = FromHex("zz")
	require.Error(t, err)
}

func TestNewPeerID(t *testing.T) {
	a, b := NewPeerID(), NewPeerID()
	require.True(t, strings.HasPrefix(a.String(), peerIDPrefix))
	require.NotEqual(t, a, b)
}
