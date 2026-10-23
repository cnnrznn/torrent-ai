package metainfo

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseMagnet(t *testing.T) {
	const hex = "7acf8fb590b2060dd9c3146ef770169d593433b0"

	t.Run("parses hex infohash, name and trackers", func(t *testing.T) {
		m, err := ParseMagnet("magnet:?xt=urn:btih:" + hex +
			"&dn=debian+13.iso" +
			"&tr=http%3A%2F%2Ftracker.example%2Fannounce" +
			"&tr=udp%3A%2F%2Ftracker.example%3A1337")
		require.NoError(t, err)
		require.Equal(t, hex, m.InfoHash.Hex())
		require.Equal(t, "debian 13.iso", m.Name)
		require.Equal(t, []string{"http://tracker.example/announce", "udp://tracker.example:1337"}, m.Trackers)
	})

	t.Run("accepts uppercase hex", func(t *testing.T) {
		m, err := ParseMagnet("magnet:?xt=urn:btih:7ACF8FB590B2060DD9C3146EF770169D593433B0")
		require.NoError(t, err)
		require.Equal(t, hex, m.InfoHash.Hex())
	})

	t.Run("parses base32 infohash", func(t *testing.T) {
		m, err := ParseMagnet("magnet:?xt=urn:btih:PLHY7NMQWIDA3WODCRXPO4AWTVMTIM5Q")
		require.NoError(t, err)
		require.Equal(t, hex, m.InfoHash.Hex())
		require.Empty(t, m.Trackers)
	})

	t.Run("rejects bad links", func(t *testing.T) {
		for _, s := range []string{
			"http://example.com",
			"magnet:?dn=x",
			"magnet:?xt=urn:btih:abc",
			"magnet:?xt=urn:btih:" + hex[:39] + "z",
			"magnet:?xt=urn:sha1:" + hex,
		} {
			_, err := ParseMagnet(s)
			require.Error(t, err, s)
		}
	})
}
