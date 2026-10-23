package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"

	"github.com/cnnrznn/torrent-ai/pkg/swarm"
	"github.com/cnnrznn/torrent-ai/pkg/torrent"
)

// otelEnv lists the variables setupTelemetry may set, so tests can
// restore them.
var otelEnv = []string{
	"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_HEADERS",
	"OTEL_EXPORTER_OTLP_INSECURE", "OTEL_TRACES_EXPORTER", "OTEL_METRICS_EXPORTER", "OTEL_LOGS_EXPORTER",
	"OTEL_SERVICE_NAME", "OTEL_RESOURCE_ATTRIBUTES", "OTEL_TRACES_SAMPLER", "OTEL_TRACES_SAMPLER_ARG",
	"OTEL_METRIC_EXPORT_INTERVAL",
}

func TestOtelConfigEnv(t *testing.T) {
	c := otelConfig{endpoint: "http://collector:4318", headers: "x-api-key=secret", sampleRatio: "0.25"}
	env := c.env()
	require.Equal(t, "http://collector:4318", env["OTEL_EXPORTER_OTLP_ENDPOINT"])
	require.Equal(t, "parentbased_traceidratio", env["OTEL_TRACES_SAMPLER"])
	require.Equal(t, "0.25", env["OTEL_TRACES_SAMPLER_ARG"])
	require.NotContains(t, env, "OTEL_EXPORTER_OTLP_PROTOCOL", "empty flags leave OTEL_* alone")

	require.Error(t, otelConfig{sampleRatio: "2"}.validate())
	require.Error(t, otelConfig{protocol: "udp"}.validate())
	require.NoError(t, otelConfig{protocol: "grpc", sampleRatio: "0.5"}.validate())
}

func TestTelemetryDisabled(t *testing.T) {
	tel, err := setupTelemetry(context.Background(), otelConfig{})
	require.NoError(t, err)
	require.Nil(t, tel.logHandler)
	require.NoError(t, tel.shutdown(context.Background()))
}

func TestTelemetryExportsAllSignals(t *testing.T) {
	for _, k := range otelEnv {
		t.Setenv(k, "")
	}
	prevTP, prevMP := otel.GetTracerProvider(), otel.GetMeterProvider()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetMeterProvider(prevMP)
	})

	var mu sync.Mutex
	got := map[string]int{}
	var headers []string
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got[r.URL.Path] += len(body)
		headers = append(headers, r.Header.Get("X-Api-Key"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	ctx := context.Background()
	tel, err := setupTelemetry(ctx, otelConfig{
		enabled:  true,
		endpoint: collector.URL,
		protocol: "http/protobuf",
		headers:  "x-api-key=secret",
	})
	require.NoError(t, err)
	require.NotNil(t, tel.logHandler)

	// One of each signal.
	_, span := otel.Tracer(instrumentationName).Start(ctx, "torrentai.run")
	span.End()
	var sess atomic.Pointer[torrent.Session]
	require.NoError(t, registerMetrics(&sess, swarm.New(swarm.Config{}), nil))
	slog.New(tel.logHandler).Info("exit", "result", "complete")

	require.NoError(t, tel.shutdown(ctx))

	mu.Lock()
	defer mu.Unlock()
	for _, path := range []string{"/v1/traces", "/v1/metrics", "/v1/logs"} {
		require.Positive(t, got[path], "%s exported", path)
	}
	require.Contains(t, headers, "secret", "headers are sent to the collector")
}
