package tracker

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"time"

	"github.com/cnnrznn/torrent-scratch/pkg/bencoding"
)

type httpTracker struct {
	announce url.URL
	client   http.Client
}

// Announce runs a tracker GET request over HTTP and returns the
// Go Response struct.
func (t *httpTracker) Announce(
	ctx context.Context,
	r Request,
) (
	*Response,
	error,
) {
	u := t.announce
	q := u.Query()
	q.Set("info_hash", r.InfoHash.String())
	q.Set("peer_id", r.PeerID.String())
	q.Set("port", fmt.Sprintf("%d", r.Port))
	q.Set("uploaded", fmt.Sprintf("%d", r.Uploaded))
	q.Set("downloaded", fmt.Sprintf("%d", r.Downloaded))
	q.Set("left", fmt.Sprintf("%d", r.Left))
	q.Set("compact", "1")
	if r.Event != EventNone {
		q.Set("event", string(r.Event))
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	res, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	bs, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	return parseResponse(bs)
}

// parseResponse parses a bencoded tracker response
// into its Go struct.
func parseResponse(bs []byte) (*Response, error) {
	d, _, err := bencoding.ParseDictionary(string(bs))
	if err != nil {
		return nil, err
	}

	if _, ok := d["failure reason"]; ok {
		failure, _, err := bencoding.ParseString(d["failure reason"])
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("tracker GET failure_reason: %s", failure)
	}

	interval, _, err := bencoding.ParseInteger(d["interval"])
	if err != nil {
		return nil, err
	}

	peersStr, _, err := bencoding.ParseString(d["peers"])
	if err != nil {
		return nil, err
	}

	peers, err := ParseCompactPeers(peersStr)
	if err != nil {
		return nil, err
	}

	return &Response{
		Interval: time.Duration(interval) * time.Second,
		Peers:    peers,
	}, nil
}

// ParseCompactPeers parses a string of 6-byte IPv4 address and port
// pairs.
func ParseCompactPeers(s string) ([]netip.AddrPort, error) {
	if len(s)%6 != 0 {
		return nil, fmt.Errorf("compact peer list length %d is not a multiple of 6", len(s))
	}

	var peers []netip.AddrPort

	for i := 0; i < len(s); i += 6 {
		bs := []byte(s[i : i+6])
		ip := netip.AddrFrom4([4]byte(bs[:4]))
		port := binary.BigEndian.Uint16(bs[4:])
		peers = append(peers, netip.AddrPortFrom(ip, port))
	}

	return peers, nil
}
