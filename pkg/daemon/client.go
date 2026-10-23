package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to a daemon's API.
type Client struct {
	base  string
	http  *http.Client
	token string
}

// ClientConfig says how to reach the daemon. Set Socket or Addr.
type ClientConfig struct {
	// Socket is the path of the daemon's unix socket.
	Socket string
	// Addr is a TCP address (host:port) or base URL (http://...).
	Addr string
	// Token is the API's bearer token, if it has one.
	Token string
	// Timeout bounds each request; defaults to 30s.
	Timeout time.Duration
}

// NewClient returns a client for the daemon described by cfg.
func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	c := &Client{token: cfg.Token, http: &http.Client{Timeout: cfg.Timeout}}
	switch {
	case cfg.Socket != "":
		socket := cfg.Socket
		c.http.Transport = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		}
		c.base = "http://torrentai"
	case cfg.Addr != "":
		c.base = cfg.Addr
		if !strings.Contains(c.base, "://") {
			c.base = "http://" + c.base
		}
		c.base = strings.TrimSuffix(c.base, "/")
	default:
		return nil, fmt.Errorf("client needs a socket or an address")
	}
	return c, nil
}

// Add adds a torrent.
func (c *Client) Add(ctx context.Context, req AddRequest) (Status, error) {
	var st Status
	err := c.do(ctx, http.MethodPost, "/v1/torrents", req, http.StatusCreated, &st)
	return st, err
}

// List returns every torrent.
func (c *Client) List(ctx context.Context) ([]Status, error) {
	var out []Status
	err := c.do(ctx, http.MethodGet, "/v1/torrents", nil, http.StatusOK, &out)
	return out, err
}

// Status returns one torrent.
func (c *Client) Status(ctx context.Context, infoHash string) (Status, error) {
	var st Status
	err := c.do(ctx, http.MethodGet, "/v1/torrents/"+url.PathEscape(infoHash), nil, http.StatusOK, &st)
	return st, err
}

// Remove removes a torrent, deleting its files if deleteFiles is set.
func (c *Client) Remove(ctx context.Context, infoHash string, deleteFiles bool) error {
	path := "/v1/torrents/" + url.PathEscape(infoHash)
	if deleteFiles {
		path += "?delete_files=true"
	}
	return c.do(ctx, http.MethodDelete, path, nil, http.StatusNoContent, nil)
}

// RemoteError is an error response from the daemon.
type RemoteError struct {
	Status int
	APIError
}

func (e *RemoteError) Error() string {
	return fmt.Sprintf("daemon: %s (HTTP %d)", e.APIError.Error, e.Status)
}

func (c *Client) do(ctx context.Context, method, path string, body any, want int, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("reaching the daemon: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode != want {
		e := &RemoteError{Status: res.StatusCode}
		if err := json.NewDecoder(res.Body).Decode(&e.APIError); err != nil {
			e.APIError.Error = http.StatusText(res.StatusCode)
		}
		return e
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}
