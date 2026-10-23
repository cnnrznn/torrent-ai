// Package tracker announces to BitTorrent trackers over HTTP (BEP 3)
// and UDP (BEP 15).
package tracker

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"time"

	"github.com/cnnrznn/torrent-ai/pkg/id"
)

// Tracker announces to a single tracker.
type Tracker interface {
	Announce(ctx context.Context, r Request) (*Response, error)
}

// Event is the reason for an announce. The zero value is a regular
// announce.
type Event string

const (
	EventNone      Event = ""
	EventStarted   Event = "started"
	EventCompleted Event = "completed"
	EventStopped   Event = "stopped"
)

type Request struct {
	InfoHash   id.Hash
	PeerID     id.Hash
	Port       int
	Uploaded   int
	Downloaded int
	Left       int
	Event      Event
	// NumWant is how many peers to ask for. Zero leaves it to the
	// tracker (BEP 15 sends -1 for that).
	NumWant int
}

type Response struct {
	Interval time.Duration
	Peers    []netip.AddrPort
}

type options struct {
	log        *slog.Logger
	httpClient http.Client
	udpTimeout time.Duration
	udpRetries int
	udpConnTTL time.Duration
}

type Option func(*options)

// WithLogger sets where requests, responses and retries are logged at
// debug level.
func WithLogger(l *slog.Logger) Option {
	return func(o *options) {
		o.log = l
	}
}

func WithHTTPClient(c http.Client) Option {
	return func(o *options) {
		o.httpClient = c
	}
}

// WithHTTPTimeout bounds each HTTP announce.
func WithHTTPTimeout(d time.Duration) Option {
	return func(o *options) {
		o.httpClient.Timeout = d
	}
}

// WithUDPRetries sets the UDP timeout schedule: the n-th attempt waits
// base * 2^n, for n up to retries. BEP 15 specifies 15s and 8.
func WithUDPRetries(base time.Duration, retries int) Option {
	return func(o *options) {
		o.udpTimeout = base
		o.udpRetries = retries
	}
}

// WithUDPConnectionIDLifetime sets how long a UDP connection ID is
// reused before connecting again. BEP 15 specifies one minute.
func WithUDPConnectionIDLifetime(d time.Duration) Option {
	return func(o *options) {
		o.udpConnTTL = d
	}
}

// Redact returns an announce URL that is safe to log: user info and
// the query string, where private trackers often put a passkey, are
// removed.
func Redact(announce string) string {
	u, err := url.Parse(announce)
	if err != nil {
		return "(invalid url)"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// New creates a tracker client, choosing HTTP or UDP from the announce
// URL's scheme.
func New(announce string, opts ...Option) (Tracker, error) {
	a, err := url.Parse(announce)
	if err != nil {
		return nil, err
	}

	o := options{
		log: slog.New(slog.DiscardHandler),
		httpClient: http.Client{
			Timeout: 10 * time.Second,
		},
		udpTimeout: 15 * time.Second,
		udpRetries: 8,
		udpConnTTL: time.Minute,
	}
	for _, opt := range opts {
		opt(&o)
	}

	switch a.Scheme {
	case "http", "https":
		return &httpTracker{announce: *a, client: o.httpClient, log: o.log}, nil
	case "udp":
		if a.Port() == "" {
			return nil, fmt.Errorf("udp tracker %q has no port", announce)
		}
		return &udpTracker{host: a.Host, timeout: o.udpTimeout, retries: o.udpRetries, connTTL: o.udpConnTTL, log: o.log}, nil
	}
	return nil, fmt.Errorf("unsupported tracker scheme %q", a.Scheme)
}
