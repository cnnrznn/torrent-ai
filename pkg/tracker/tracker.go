package tracker

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/cnnrznn/torrent-scratch/pkg/bencoding"
	"github.com/cnnrznn/torrent-scratch/pkg/id"
)

type Tracker struct {
	announce url.URL
	client   http.Client
}

type Request struct {
	InfoHash   id.Hash
	PeerID     id.Hash
	Port       int
	Uploaded   int
	Downloaded int
	Left       int
}

type Response struct {
	Interval time.Duration
	Peers    []net.Addr
}

type Option func(*Tracker)

func WithHTTPClient(c http.Client) Option {
	return func(t *Tracker) {
		t.client = c
	}
}

// New creates a new torrent tracker client.
func New(announce string, opts ...Option) (*Tracker, error) {
	a, err := url.Parse(announce)
	if err != nil {
		return nil, err
	}

	tracker := &Tracker{
		announce: *a,
		client: http.Client{
			Timeout: 10 * time.Second,
		},
	}

	for _, o := range opts {
		o(tracker)
	}

	return tracker, nil
}

// Get runs a tracker GET request over HTTP and returns the
// Go Response struct.
func (t *Tracker) Get(
	r Request,
) (
	*Response,
	error,
) {
	u := t.announce
	q := url.Values{}
	q.Set("info_hash", r.InfoHash.String())
	q.Set("peer_id", r.PeerID.String())
	q.Set("port", fmt.Sprintf("%d", r.Port))
	q.Set("uploaded", fmt.Sprintf("%d", r.Uploaded))
	q.Set("downloaded", fmt.Sprintf("%d", r.Downloaded))
	q.Set("compact", "1")
	u.RawQuery = q.Encode()

	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	res, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	bs, err := io.ReadAll(res.Body)
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

	var peers []net.Addr

	for i := 0; i < len(peersStr); i += 6 {
		bs := []byte(peersStr[i : i+6])
		ip := net.IP(bs[:4])
		port := binary.BigEndian.Uint16(bs[4:])
		peers = append(peers, &net.TCPAddr{
			IP:   ip,
			Port: int(port),
		})
	}

	return &Response{
		Interval: time.Duration(interval) * time.Second,
		Peers:    peers,
	}, nil
}
