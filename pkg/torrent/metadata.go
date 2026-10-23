package torrent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"sync"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/cnnrznn/torrent-ai/pkg/id"
	"github.com/cnnrznn/torrent-ai/pkg/metainfo"
	"github.com/cnnrznn/torrent-ai/pkg/peerwire"
	"github.com/cnnrznn/torrent-ai/pkg/swarm"
)

// MetadataConfig tunes FetchMetadata. Zero fields take the defaults
// in DefaultMetadataConfig.
type MetadataConfig struct {
	// Logger receives progress and per-attempt logs. Defaults to
	// discarding.
	Logger *slog.Logger
	// Parallel is how many peers are tried at once.
	Parallel int
	// Timeout bounds each attempt, from dialing to a verified info
	// dict.
	Timeout time.Duration
	// Heartbeat is how often "metadata waiting" is logged.
	Heartbeat time.Duration
	// Tick is how often the swarm is checked for addresses whose
	// backoff has ended.
	Tick time.Duration
	// Conn tunes peer connections.
	Conn peerwire.Options
	// Metadata tunes the metadata exchange itself.
	Metadata peerwire.MetadataOptions
}

// DefaultMetadataConfig returns the default settings.
func DefaultMetadataConfig() MetadataConfig {
	return MetadataConfig{
		Parallel:  8,
		Timeout:   30 * time.Second,
		Heartbeat: 10 * time.Second,
		Tick:      time.Second,
	}
}

// FetchMetadata gets the info dict for a magnet link from peers in the
// swarm, trying several at once. It waits for peer sources to add
// addresses until one succeeds or ctx is cancelled.
//
// While it waits it logs "metadata waiting" every heartbeat with how
// many attempts failed and why, so a stalled fetch can be diagnosed
// from info-level logs.
func FetchMetadata(ctx context.Context, sw *swarm.Swarm, infoHash, peerID id.Hash, cfg MetadataConfig) (*metainfo.Metainfo, error) {
	d := DefaultMetadataConfig()
	setDefault(&cfg.Parallel, d.Parallel)
	setDefault(&cfg.Timeout, d.Timeout)
	setDefault(&cfg.Heartbeat, d.Heartbeat)
	setDefault(&cfg.Tick, d.Tick)
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	ctx, span := tracer().Start(ctx, "metadata.fetch", trace.WithAttributes(attribute.String("torrent.infohash", infoHash.Hex())))
	var fetchErr error
	defer func() { endSpan(span, fetchErr) }()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type success struct {
		raw  string
		addr netip.AddrPort
	}
	results := make(chan success, 1)
	slots := make(chan struct{}, cfg.Parallel)

	var mu sync.Mutex
	attempts := 0
	failures := map[string]int{}

	start := time.Now()
	ticker := time.NewTicker(cfg.Tick)
	defer ticker.Stop()
	heartbeat := time.NewTicker(cfg.Heartbeat)
	defer heartbeat.Stop()

	for {
		for len(slots) < cap(slots) {
			addr, ok := sw.Next()
			if !ok {
				break
			}
			slots <- struct{}{}
			mu.Lock()
			attempts++
			mu.Unlock()

			go func() {
				defer func() { <-slots }()
				actx, aspan := tracer().Start(ctx, "metadata.attempt", trace.WithAttributes(attribute.String("peer.addr", addr.String())))
				attemptStart := time.Now()
				raw, err := fetchMetadataFrom(actx, addr, infoHash, peerID, cfg)
				sw.Release(addr, err != nil)
				if err != nil {
					if ctx.Err() != nil {
						aspan.SetAttributes(attribute.String("metadata.result", "cancelled"))
						aspan.End()
						return
					}
					reason := failureReason(err)
					aspan.SetAttributes(attribute.String("metadata.result", "failed"), attribute.String("metadata.reason", reason))
					endSpan(aspan, err)
					mu.Lock()
					failures[reason]++
					mu.Unlock()
					log.Debug("metadata attempt", "addr", addr, "result", "failed", "reason", reason, "err", err, "dur", time.Since(attemptStart))
					return
				}
				aspan.SetAttributes(attribute.String("metadata.result", "ok"), attribute.Int("metadata.size", len(raw)))
				aspan.End()
				log.Debug("metadata attempt", "addr", addr, "result", "ok", "dur", time.Since(attemptStart))
				select {
				case results <- success{raw, addr}:
				default:
				}
			}()
		}

		select {
		case r := <-results:
			m, err := metainfo.FromInfoDict(r.raw)
			if err != nil {
				fetchErr = err
				return nil, err
			}
			mu.Lock()
			span.SetAttributes(
				attribute.String("metadata.from", r.addr.String()),
				attribute.Int("metadata.size", len(r.raw)),
				attribute.Int("metadata.attempts", attempts),
				attribute.String("torrent.name", m.Name()),
			)
			log.Info("metadata fetched", "from", r.addr, "size", len(r.raw), "name", m.Name(), "attempts", attempts, "dur", time.Since(start).Round(time.Millisecond))
			mu.Unlock()
			return m, nil
		case <-ctx.Done():
			fetchErr = ctx.Err()
			return nil, ctx.Err()
		case <-sw.Notify():
		case <-ticker.C:
		case <-heartbeat.C:
			mu.Lock()
			attrs := []any{
				"elapsed", time.Since(start).Round(time.Second),
				"known", sw.Len(),
				"in_flight", len(slots),
				"attempts", attempts,
			}
			for _, reason := range failureReasons {
				if n := failures[reason]; n > 0 {
					attrs = append(attrs, "failed_"+reason, n)
				}
			}
			mu.Unlock()
			log.Info("metadata waiting", attrs...)
		}
	}
}

var failureReasons = []string{"dial", "no_extensions", "no_ut_metadata", "rejected", "bad_hash", "timeout", "closed", "protocol"}

// failureReason classifies why a metadata attempt failed.
func failureReason(err error) string {
	var dialErr *dialError
	switch {
	case errors.As(err, &dialErr):
		return "dial"
	case errors.Is(err, peerwire.ErrNoExtensions):
		return "no_extensions"
	case errors.Is(err, peerwire.ErrNoUTMetadata):
		return "no_ut_metadata"
	case errors.Is(err, peerwire.ErrMetadataRejected):
		return "rejected"
	case errors.Is(err, peerwire.ErrMetadataHash):
		return "bad_hash"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, peerwire.ErrClosed), errors.Is(err, io.EOF), errors.Is(err, syscall.ECONNRESET):
		return "closed"
	}
	return "protocol"
}

// dialError marks a failure to connect or handshake, as opposed to a
// failure once connected.
type dialError struct{ err error }

func (e *dialError) Error() string { return "dial: " + e.err.Error() }
func (e *dialError) Unwrap() error { return e.err }

func fetchMetadataFrom(ctx context.Context, addr netip.AddrPort, infoHash, peerID id.Hash, cfg MetadataConfig) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	c, err := peerwire.Dial(ctx, addr, peerwire.NewHandshake(infoHash, peerID, false), cfg.Conn)
	if err != nil {
		return "", &dialError{err}
	}
	defer c.Close()

	return peerwire.FetchMetadata(ctx, c, infoHash, cfg.Metadata)
}
