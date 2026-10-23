package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/cnnrznn/torrent-ai/pkg/daemon"
)

func TestDaemonTelemetry(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	rec := tracetest.NewSpanRecorder()
	prevMP, prevTP := otel.GetMeterProvider(), otel.GetTracerProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() {
		otel.SetMeterProvider(prevMP)
		otel.SetTracerProvider(prevTP)
	})

	ctx, cancel := context.WithCancel(context.Background())
	dir := t.TempDir()
	torrentFile, meta, data := writeTestTorrent(t, dir, 50_000)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "data.bin"), data, 0o644))
	file, err := os.ReadFile(torrentFile)
	require.NoError(t, err)

	m := daemon.New(daemon.Config{})
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	_, err = m.Add(daemon.Source{Torrent: file, Dir: dir})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		st, _ := m.Status(meta.InfoHash())
		return st.State == daemon.StateSeeding
	}, 10*time.Second, 10*time.Millisecond)

	collect := func() map[string]metricdata.Metrics {
		var rm metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(ctx, &rm))
		out := map[string]metricdata.Metrics{}
		for _, sm := range rm.ScopeMetrics {
			for _, mm := range sm.Metrics {
				out[mm.Name] = mm
			}
		}
		return out
	}

	t.Run("per torrent", func(t *testing.T) {
		require.NoError(t, registerDaemonMetrics(m, nil, true))
		got := collect()

		verified := got["torrent.pieces.verified"].Data.(metricdata.Gauge[int64]).DataPoints
		require.Len(t, verified, 1)
		require.Equal(t, int64(meta.NumPieces()), verified[0].Value)
		hash, _ := verified[0].Attributes.Value("torrent.infohash")
		require.Equal(t, meta.InfoHash().Hex(), hash.AsString())

		var seeding int64
		for _, dp := range got["daemon.torrents"].Data.(metricdata.Gauge[int64]).DataPoints {
			if v, _ := dp.Attributes.Value("torrent.state"); v.AsString() == "seeding" {
				seeding = dp.Value
			}
		}
		require.EqualValues(t, 1, seeding)
	})

	// The torrent's span covers its life and ends when it's removed.
	require.NoError(t, m.Remove(meta.InfoHash(), false))
	cancel()
	<-done

	var torrentSpan sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.Name() == "daemon.torrent" {
			torrentSpan = s
		}
	}
	require.NotNil(t, torrentSpan)
	require.Contains(t, torrentSpan.Attributes(), attribute.String("torrent.name", "data.bin"))
	var sessionParent bool
	for _, s := range rec.Ended() {
		if s.Name() == "torrent.session" && s.Parent().SpanID() == torrentSpan.SpanContext().SpanID() {
			sessionParent = true
		}
	}
	require.True(t, sessionParent, "the session is a child of the torrent's span")
}

func TestDaemonMetricsAggregate(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	m := daemon.New(daemon.Config{})
	require.NoError(t, registerDaemonMetrics(m, nil, false))
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	for _, sm := range rm.ScopeMetrics {
		for _, mm := range sm.Metrics {
			if mm.Name == "torrent.left" {
				dps := mm.Data.(metricdata.Gauge[int64]).DataPoints
				require.Len(t, dps, 1)
				require.Zero(t, dps[0].Attributes.Len(), "aggregated series have no torrent labels")
			}
		}
	}
}
