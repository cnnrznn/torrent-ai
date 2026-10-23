package daemon

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"path/filepath"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// maxRequestBody bounds an API request; it fits a .torrent file with a
// large info dict, base64-encoded.
const maxRequestBody = 32 << 20

// AddRequest is the body of POST /v1/torrents. Set Magnet or Torrent.
type AddRequest struct {
	Magnet   string   `json:"magnet,omitempty"`
	Torrent  []byte   `json:"torrent,omitempty"` // .torrent file, base64 in JSON
	Dir      string   `json:"dir,omitempty"`     // defaults to the daemon's download dir
	Peers    []string `json:"peers,omitempty"`   // host:port addresses to try directly
	Trackers []string `json:"trackers,omitempty"`
}

// APIError is the body of an error response.
type APIError struct {
	Error    string `json:"error"`
	InfoHash string `json:"infohash,omitempty"`
}

// APIConfig configures the API handler.
type APIConfig struct {
	// Token, if set, must be sent as "Authorization: Bearer <token>"
	// on /v1 requests. Health checks are always open.
	Token string
	// DefaultDir is where torrents go when a request names no dir.
	DefaultDir string
	// Ready reports whether the daemon can serve: listener bound and,
	// with the DHT on, enough routing table nodes. The reason is
	// returned when it isn't.
	Ready func() (bool, string)
	// Logger receives a debug line per request.
	Logger *slog.Logger
}

// NewAPI returns the daemon's HTTP API over m.
//
//	POST   /v1/torrents             add (AddRequest) -> Status
//	GET    /v1/torrents             list -> []Status
//	GET    /v1/torrents/{infohash}  one -> Status
//	DELETE /v1/torrents/{infohash}  remove; ?delete_files=true deletes data
//	GET    /healthz                 the process is up
//	GET    /readyz                  the daemon can serve
func NewAPI(m *Manager, cfg APIConfig) http.Handler {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	a := &api{m: m, cfg: cfg}

	v1 := http.NewServeMux()
	v1.HandleFunc("POST /v1/torrents", a.add)
	v1.HandleFunc("GET /v1/torrents", a.list)
	v1.HandleFunc("GET /v1/torrents/{infohash}", a.get)
	v1.HandleFunc("DELETE /v1/torrents/{infohash}", a.remove)

	mux := http.NewServeMux()
	mux.Handle("/v1/", a.authorize(v1))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", a.ready)
	return a.observe(mux)
}

type api struct {
	m   *Manager
	cfg APIConfig
}

func (a *api) authorize(next http.Handler) http.Handler {
	if a.cfg.Token == "" {
		return next
	}
	want := []byte("Bearer " + a.cfg.Token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			writeJSON(w, http.StatusUnauthorized, APIError{Error: "missing or wrong bearer token"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// observe wraps each request in a span and a debug log line.
func (a *api) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ctx, span := otel.Tracer("github.com/cnnrznn/torrent-ai/pkg/daemon").Start(r.Context(), "api "+r.Method+" "+routeName(r.URL.Path),
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attribute.String("http.request.method", r.Method), attribute.String("url.path", r.URL.Path)))
		defer span.End()

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r.WithContext(ctx))

		span.SetAttributes(attribute.Int("http.response.status_code", rec.status))
		if rec.status >= 500 {
			span.SetStatus(codes.Error, http.StatusText(rec.status))
		}
		a.cfg.Logger.Debug("api request", "method", r.Method, "path", r.URL.Path, "status", rec.status, "dur", time.Since(start))
	})
}

// routeName collapses an infohash in the path, to keep span names
// low-cardinality.
func routeName(path string) string {
	if strings.HasPrefix(path, "/v1/torrents/") {
		return "/v1/torrents/{infohash}"
	}
	return path
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (a *api) add(w http.ResponseWriter, r *http.Request) {
	var req AddRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, APIError{Error: "invalid request body: " + err.Error()})
		return
	}

	src := Source{Magnet: req.Magnet, Torrent: req.Torrent, Dir: req.Dir, Trackers: req.Trackers}
	if src.Dir == "" {
		src.Dir = a.cfg.DefaultDir
	}
	if !filepath.IsAbs(src.Dir) {
		writeJSON(w, http.StatusBadRequest, APIError{Error: "dir must be an absolute path"})
		return
	}
	for _, p := range req.Peers {
		addr, err := netip.ParseAddrPort(p)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, APIError{Error: "invalid peer " + p + ": " + err.Error()})
			return
		}
		src.Peers = append(src.Peers, addr)
	}

	h, err := a.m.Add(src)
	switch {
	case errors.Is(err, ErrExists):
		writeJSON(w, http.StatusConflict, APIError{Error: err.Error(), InfoHash: h.Hex()})
		return
	case err != nil:
		writeJSON(w, http.StatusBadRequest, APIError{Error: err.Error()})
		return
	}
	st, err := a.m.Status(h)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, APIError{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, st)
}

func (a *api) list(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.m.List())
}

func (a *api) get(w http.ResponseWriter, r *http.Request) {
	h, err := ParseInfoHash(r.PathValue("infohash"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, APIError{Error: err.Error()})
		return
	}
	st, err := a.m.Status(h)
	if errors.Is(err, ErrNotFound) {
		writeJSON(w, http.StatusNotFound, APIError{Error: err.Error(), InfoHash: h.Hex()})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (a *api) remove(w http.ResponseWriter, r *http.Request) {
	h, err := ParseInfoHash(r.PathValue("infohash"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, APIError{Error: err.Error()})
		return
	}
	err = a.m.Remove(h, r.URL.Query().Get("delete_files") == "true")
	switch {
	case errors.Is(err, ErrNotFound):
		writeJSON(w, http.StatusNotFound, APIError{Error: err.Error(), InfoHash: h.Hex()})
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, APIError{Error: err.Error(), InfoHash: h.Hex()})
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (a *api) ready(w http.ResponseWriter, r *http.Request) {
	if a.cfg.Ready != nil {
		if ok, reason := a.cfg.Ready(); !ok {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready", "reason": reason})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
