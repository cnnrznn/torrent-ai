package pyroscope

import (
	"context"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// upload is what the fake server saw.
type upload struct {
	name, format, user, pass, tenant string
	from, until                      int64
	gzip                             bool
}

func fakeServer(t *testing.T, tlsServer bool) (*httptest.Server, func() []upload) {
	t.Helper()
	var mu sync.Mutex
	var got []upload
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ingest" || r.Method != http.MethodPost {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		// Like Pyroscope 2.3.2: the raw body must be the profile.
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			http.Error(w, "parsing IngestInput-pprof failed", http.StatusUnprocessableEntity)
			return
		}
		b, _ := io.ReadAll(r.Body)
		q := r.URL.Query()
		from, _ := strconv.ParseInt(q.Get("from"), 10, 64)
		until, _ := strconv.ParseInt(q.Get("until"), 10, 64)
		user, pass, _ := r.BasicAuth()
		mu.Lock()
		got = append(got, upload{
			name: q.Get("name"), format: q.Get("format"),
			user: user, pass: pass, tenant: r.Header.Get("X-Scope-OrgID"),
			from: from, until: until,
			gzip: len(b) > 2 && b[0] == 0x1f && b[1] == 0x8b,
		})
		mu.Unlock()
	})
	var srv *httptest.Server
	if tlsServer {
		srv = httptest.NewTLSServer(h)
	} else {
		srv = httptest.NewServer(h)
	}
	t.Cleanup(srv.Close)
	return srv, func() []upload {
		mu.Lock()
		defer mu.Unlock()
		return append([]upload(nil), got...)
	}
}

// runOnce runs an uploader for one short interval plus the final
// upload.
func runOnce(t *testing.T, cfg Config) {
	t.Helper()
	cfg.Interval = 100 * time.Millisecond
	u, err := New(cfg)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	u.Run(ctx)
}

func TestUpload(t *testing.T) {
	t.Run("no auth, plain http", func(t *testing.T) {
		srv, got := fakeServer(t, false)
		runOnce(t, Config{URL: srv.URL, App: "torrentai", Labels: map[string]string{"pod": "p-1", "env": "home"}})

		ups := got()
		require.GreaterOrEqual(t, len(ups), 4, "cpu and heap per interval, plus the final pair")
		for _, up := range ups {
			require.Equal(t, "torrentai{env=home,pod=p-1}", up.name)
			require.Equal(t, "pprof", up.format)
			require.True(t, up.gzip, "pprof profiles are gzipped")
			require.LessOrEqual(t, up.from, up.until)
			require.InDelta(t, time.Now().Unix(), up.until, 5, "unix seconds")
			require.Empty(t, up.user)
			require.Empty(t, up.tenant)
		}
	})

	t.Run("basic auth and tenant", func(t *testing.T) {
		srv, got := fakeServer(t, false)
		runOnce(t, Config{URL: srv.URL, User: "123456", Password: "glc_token", TenantID: "team-a"})
		up := got()[0]
		require.Equal(t, "123456", up.user)
		require.Equal(t, "glc_token", up.pass)
		require.Equal(t, "team-a", up.tenant)
	})

	t.Run("tls with a custom CA", func(t *testing.T) {
		srv, got := fakeServer(t, true)
		ca := filepath.Join(t.TempDir(), "ca.pem")
		require.NoError(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o644))
		runOnce(t, Config{URL: srv.URL, CAFile: ca})
		require.NotEmpty(t, got())
	})

	t.Run("tls skip verify", func(t *testing.T) {
		srv, got := fakeServer(t, true)
		runOnce(t, Config{URL: srv.URL, InsecureSkipVerify: true})
		require.NotEmpty(t, got())
	})

	t.Run("an unknown CA fails", func(t *testing.T) {
		srv, got := fakeServer(t, true)
		runOnce(t, Config{URL: srv.URL})
		require.Empty(t, got())
	})
}

func TestNewValidates(t *testing.T) {
	for _, cfg := range []Config{
		{URL: "ftp://x"},
		{URL: "not a url"},
		{URL: "http://x", App: "bad name"},
		{URL: "http://x", Labels: map[string]string{"k": "a,b"}},
		{URL: "http://x", Labels: map[string]string{"bad key": "v"}},
		{URL: "https://x", CAFile: "/nonexistent"},
	} {
		_, err := New(cfg)
		require.Error(t, err, "%+v", cfg)
	}
	labels := DefaultLabels("v1.2.3")
	require.Equal(t, "v1.2.3", labels["version"])
	require.NotEmpty(t, labels["go_version"])
}
