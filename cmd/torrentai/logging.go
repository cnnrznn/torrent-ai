package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// Components tag every log line with "component", so output can be
// filtered by subsystem and debug logging enabled per subsystem.
const (
	compMain     = "main"
	compSession  = "session"
	compMetadata = "metadata"
	compTracker  = "tracker"
	compDHT      = "dht"
)

var components = []string{compMain, compSession, compMetadata, compTracker, compDHT}

// newLogger returns a logger writing text or JSON to w, and to extra
// when it isn't nil (the OTel log pipeline). Records are logged at info
// and above, plus debug for the components named in debug ("all" for
// every component).
func newLogger(w io.Writer, format string, debug []string, extra slog.Handler) (*slog.Logger, error) {
	opts := &slog.HandlerOptions{
		Level: slog.LevelDebug,
		// Durations read as "1.5s" rather than nanoseconds in JSON.
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Value.Kind() == slog.KindDuration {
				return slog.String(a.Key, a.Value.Duration().String())
			}
			return a
		},
	}

	var inner slog.Handler
	switch format {
	case "text":
		inner = slog.NewTextHandler(w, opts)
	case "json":
		inner = slog.NewJSONHandler(w, opts)
	default:
		return nil, fmt.Errorf("unknown log format %q (want text or json)", format)
	}

	enabled := map[string]bool{}
	for _, c := range debug {
		if c == "" {
			continue
		}
		if c != "all" && !isComponent(c) {
			return nil, fmt.Errorf("unknown debug component %q (want all or %s)", c, strings.Join(components, ","))
		}
		enabled[c] = true
	}

	if extra != nil {
		inner = teeHandler{inner, extra}
	}
	return slog.New(&componentHandler{inner: inner, debug: enabled}), nil
}

// teeHandler sends each record to every handler that wants it.
type teeHandler []slog.Handler

func (t teeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range t {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (t teeHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range t {
		if h.Enabled(ctx, r.Level) {
			errs = append(errs, h.Handle(ctx, r.Clone()))
		}
	}
	return errors.Join(errs...)
}

func (t teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(teeHandler, len(t))
	for i, h := range t {
		out[i] = h.WithAttrs(attrs)
	}
	return out
}

func (t teeHandler) WithGroup(name string) slog.Handler {
	out := make(teeHandler, len(t))
	for i, h := range t {
		out[i] = h.WithGroup(name)
	}
	return out
}

func isComponent(c string) bool {
	for _, x := range components {
		if x == c {
			return true
		}
	}
	return false
}

// componentHandler enables debug records only for selected components.
// It learns a logger's component from the "component" attribute added
// with Logger.With.
type componentHandler struct {
	inner     slog.Handler
	debug     map[string]bool
	component string
}

func (h *componentHandler) Enabled(_ context.Context, l slog.Level) bool {
	if l >= slog.LevelInfo {
		return true
	}
	return h.debug["all"] || h.debug[h.component]
}

func (h *componentHandler) Handle(ctx context.Context, r slog.Record) error {
	return h.inner.Handle(ctx, r)
}

func (h *componentHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := h.component
	for _, a := range attrs {
		if a.Key == "component" {
			c = a.Value.String()
		}
	}
	return &componentHandler{inner: h.inner.WithAttrs(attrs), debug: h.debug, component: c}
}

func (h *componentHandler) WithGroup(name string) slog.Handler {
	return &componentHandler{inner: h.inner.WithGroup(name), debug: h.debug, component: h.component}
}
