package tracker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/cnnrznn/torrent-ai/pkg/bencoding"
	"github.com/cnnrznn/torrent-ai/pkg/id"
)

func TestAnnounce(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		w.Write([]byte(bencoding.EncodeDictionary(map[string]string{
			"interval": bencoding.EncodeInteger(1800),
			"peers":    bencoding.EncodeString("\x7f\x00\x00\x01\x1a\xe1\x0a\x00\x00\x02\x00\x50"),
		})))
	}))
	defer srv.Close()

	tr, err := New(srv.URL + "/announce?passkey=abc")
	require.NoError(t, err)

	infoHash, peerID := id.Random(), id.NewPeerID()
	res, err := tr.Announce(context.Background(), Request{
		InfoHash: infoHash,
		PeerID:   peerID,
		Port:     6881,
		Left:     1000,
		Event:    EventStarted,
	})
	require.NoError(t, err)

	require.Equal(t, infoHash.String(), got.Get("info_hash"))
	require.Equal(t, peerID.String(), got.Get("peer_id"))
	require.Equal(t, "1000", got.Get("left"))
	require.Equal(t, "started", got.Get("event"))
	require.Equal(t, "abc", got.Get("passkey"))

	require.Equal(t, 30*time.Minute, res.Interval)
	require.Equal(t, []netip.AddrPort{
		netip.MustParseAddrPort("127.0.0.1:6881"),
		netip.MustParseAddrPort("10.0.0.2:80"),
	}, res.Peers)
}

func TestParseResponse(t *testing.T) {
	t.Run("returns the failure reason", func(t *testing.T) {
		_, err := parseResponse([]byte("d14:failure reason6:no waye"))
		require.ErrorContains(t, err, "no way")
	})

	t.Run("rejects a truncated peer list", func(t *testing.T) {
		_, err := parseResponse([]byte("d8:intervali60e5:peers5:abcdee"))
		require.ErrorContains(t, err, "not a multiple of 6")
	})
}

func TestRedact(t *testing.T) {
	require.Equal(t, "http://tracker.example/announce", Redact("http://user:pw@tracker.example/announce?passkey=secret"))
	require.Equal(t, "udp://tracker.example:1337/announce", Redact("udp://tracker.example:1337/announce"))
}
