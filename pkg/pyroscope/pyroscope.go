// Package pyroscope uploads continuous CPU and heap profiles to a
// Pyroscope server (or Grafana Cloud Profiles) through its /ingest API.
package pyroscope

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"runtime/pprof"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Config configures an Uploader.
type Config struct {
	// URL is the server's base URL, e.g. http://pyroscope:4040 or
	// https://profiles-prod-001.grafana.net. The scheme decides TLS.
	URL string
	// App is the application name; Pyroscope appends the profile type
	// (app.cpu, app.alloc_space, ...).
	App string
	// Labels are attached to every profile, e.g. pod and version.
	Labels map[string]string
	// Interval is how long each CPU profile records before upload.
	Interval time.Duration

	// Auth, both optional. Grafana Cloud uses basic auth with the
	// stack's profiles instance ID and an access policy token;
	// multi-tenant self-hosted servers use a tenant ID.
	User, Password string
	TenantID       string

	// CAFile adds a CA for a self-signed server certificate.
	CAFile string
	// InsecureSkipVerify disables certificate verification.
	InsecureSkipVerify bool

	Timeout time.Duration
	Logger  *slog.Logger
}

// Uploader records and uploads profiles until its context ends.
type Uploader struct {
	cfg    Config
	ingest string
	client *http.Client
	log    *slog.Logger
}

// New validates cfg and returns an Uploader.
func New(cfg Config) (*Uploader, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("pyroscope url %q must be http:// or https:// with a host", cfg.URL)
	}
	if cfg.App == "" {
		cfg.App = "torrentai"
	}
	if !validName.MatchString(cfg.App) {
		return nil, fmt.Errorf("pyroscope app name %q may only contain letters, digits, '.', '_' and '-'", cfg.App)
	}
	for k, v := range cfg.Labels {
		if !validName.MatchString(k) || strings.ContainsAny(v, "{},=") {
			return nil, fmt.Errorf("pyroscope label %q=%q: keys may only contain letters, digits, '.', '_' and '-', and values no braces, commas or '='", k, v)
		}
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Minute
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}

	tlsCfg := &tls.Config{InsecureSkipVerify: cfg.InsecureSkipVerify}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("pyroscope ca file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("pyroscope ca file %s has no PEM certificates", cfg.CAFile)
		}
		tlsCfg.RootCAs = pool
	}

	return &Uploader{
		cfg:    cfg,
		ingest: strings.TrimSuffix(cfg.URL, "/") + "/ingest",
		client: &http.Client{
			Timeout:   cfg.Timeout,
			Transport: &http.Transport{TLSClientConfig: tlsCfg, Proxy: http.ProxyFromEnvironment},
		},
		log: cfg.Logger,
	}, nil
}

var validName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// name is the application name with its labels, as Pyroscope expects:
// app{key=value,...}.
func (u *Uploader) name() string {
	keys := make([]string, 0, len(u.cfg.Labels))
	for k := range u.cfg.Labels {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	pairs := make([]string, len(keys))
	for i, k := range keys {
		pairs[i] = k + "=" + u.cfg.Labels[k]
	}
	return u.cfg.App + "{" + strings.Join(pairs, ",") + "}"
}

// Run records a CPU profile each interval and uploads it with a heap
// profile, until ctx ends; then it uploads what it has and returns.
// If the CPU profiler is already in use (one process can only run one),
// it uploads heap profiles alone and says so.
func (u *Uploader) Run(ctx context.Context) {
	u.log.Info("pyroscope uploads started", "url", u.ingest, "name", u.name(), "interval", u.cfg.Interval,
		"auth", u.authMode(), "tls_skip_verify", u.cfg.InsecureSkipVerify)
	if u.cfg.InsecureSkipVerify {
		u.log.Warn("pyroscope tls verification disabled", "hint", "use -pyroscope-ca-file for a self-signed certificate")
	}

	cpuWarned := false
	for {
		var cpu bytes.Buffer
		from := time.Now()
		cpuOK := pprof.StartCPUProfile(&cpu) == nil
		if !cpuOK && !cpuWarned {
			u.log.Warn("pyroscope cpu profiles unavailable", "reason", "the CPU profiler is already in use, likely by -flame-cpu-file", "hint", "heap profiles are still uploaded")
			cpuWarned = true
		}

		timer := time.NewTimer(u.cfg.Interval)
		stopping := false
		select {
		case <-ctx.Done():
			stopping = true
			timer.Stop()
		case <-timer.C:
		}
		if cpuOK {
			pprof.StopCPUProfile()
		}
		until := time.Now()

		// The final upload runs after ctx is done, with its own deadline.
		upCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), u.cfg.Timeout)
		if cpuOK {
			u.upload(upCtx, "cpu", cpu.Bytes(), from, until)
		}
		var heap bytes.Buffer
		if err := pprof.Lookup("heap").WriteTo(&heap, 0); err == nil {
			u.upload(upCtx, "heap", heap.Bytes(), from, until)
		}
		cancel()

		if stopping {
			u.log.Info("pyroscope uploads stopped")
			return
		}
	}
}

func (u *Uploader) authMode() string {
	var modes []string
	if u.cfg.User != "" || u.cfg.Password != "" {
		modes = append(modes, "basic")
	}
	if u.cfg.TenantID != "" {
		modes = append(modes, "tenant")
	}
	if len(modes) == 0 {
		return "none"
	}
	return strings.Join(modes, "+")
}

// upload sends one pprof profile to /ingest.
func (u *Uploader) upload(ctx context.Context, kind string, profile []byte, from, until time.Time) {
	start := time.Now()
	err := u.post(ctx, profile, from, until)
	if err != nil {
		u.log.Warn("pyroscope upload failed", "profile", kind, "err", err, "dur", time.Since(start).Round(time.Millisecond))
		return
	}
	u.log.Debug("pyroscope upload done", "profile", kind, "bytes", len(profile), "dur", time.Since(start).Round(time.Millisecond))
}

// post sends the gzipped pprof bytes as the raw request body. The docs
// also describe a multipart form with a "profile" field, but Pyroscope
// 2.3.2 fails to parse that ("proto: wrong wireType = 5 for field
// Function") while it accepts the raw body.
func (u *Uploader) post(ctx context.Context, profile []byte, from, until time.Time) error {
	q := url.Values{}
	q.Set("name", u.name())
	q.Set("from", strconv.FormatInt(from.Unix(), 10))
	q.Set("until", strconv.FormatInt(until.Unix(), 10))
	q.Set("format", "pprof")
	q.Set("spyName", "gospy")
	q.Set("sampleRate", "100")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.ingest+"?"+q.Encode(), bytes.NewReader(profile))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if u.cfg.User != "" || u.cfg.Password != "" {
		req.SetBasicAuth(u.cfg.User, u.cfg.Password)
	}
	if u.cfg.TenantID != "" {
		req.Header.Set("X-Scope-OrgID", u.cfg.TenantID)
	}

	res, err := u.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// DefaultLabels identifies this process: its host (the pod name in
// Kubernetes) and the Go version.
func DefaultLabels(version string) map[string]string {
	labels := map[string]string{"go_version": strings.TrimPrefix(runtime.Version(), "go")}
	if h, err := os.Hostname(); err == nil {
		labels["hostname"] = sanitize(h)
	}
	if version != "" {
		labels["version"] = sanitize(version)
	}
	return labels
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune("{},=", r) {
			return '_'
		}
		return r
	}, s)
}
