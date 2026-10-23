package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNewLogger(t *testing.T) {
	t.Run("enables debug only for selected components", func(t *testing.T) {
		var buf bytes.Buffer
		log, err := newLogger(&buf, "text", []string{"dht"}, nil)
		require.NoError(t, err)

		log.With("component", "dht").Debug("dht detail")
		log.With("component", "session").Debug("session detail")
		log.With("component", "session").Info("session summary")

		out := buf.String()
		require.Contains(t, out, "dht detail")
		require.NotContains(t, out, "session detail")
		require.Contains(t, out, "session summary")
	})

	t.Run("all enables every component", func(t *testing.T) {
		var buf bytes.Buffer
		log, err := newLogger(&buf, "text", []string{"all"}, nil)
		require.NoError(t, err)
		log.With("component", "tracker").Debug("tracker detail")
		require.Contains(t, buf.String(), "tracker detail")
	})

	t.Run("writes one JSON object per line", func(t *testing.T) {
		var buf bytes.Buffer
		log, err := newLogger(&buf, "json", nil, nil)
		require.NoError(t, err)
		log.With("component", "main").Info("exit", "result", "complete", "elapsed", 1500*time.Millisecond)

		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &rec))
		require.Equal(t, "exit", rec["msg"])
		require.Equal(t, "main", rec["component"])
		require.Equal(t, "complete", rec["result"])
		require.Equal(t, "1.5s", rec["elapsed"])
	})

	t.Run("tees to an extra handler with the same filtering", func(t *testing.T) {
		var buf, extra bytes.Buffer
		log, err := newLogger(&buf, "text", []string{"dht"}, slog.NewTextHandler(&extra, &slog.HandlerOptions{Level: slog.LevelDebug}))
		require.NoError(t, err)
		log.With("component", "dht").Debug("dht detail")
		log.With("component", "session").Debug("session detail")
		require.Contains(t, extra.String(), "dht detail")
		require.Contains(t, extra.String(), "component=dht")
		require.NotContains(t, extra.String(), "session detail")
		require.Contains(t, buf.String(), "dht detail")
	})

	t.Run("rejects unknown settings", func(t *testing.T) {
		_, err := newLogger(&bytes.Buffer{}, "xml", nil, nil)
		require.Error(t, err)
		_, err = newLogger(&bytes.Buffer{}, "text", []string{"peers"}, nil)
		require.ErrorContains(t, err, "unknown debug component")
	})
}
