package torrent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/cnnrznn/torrent-ai/pkg/id"
	"github.com/cnnrznn/torrent-ai/pkg/swarm"
)

func TestSpans(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	m, data := makeTorrent(t, 32*1024, 100_000)
	seedDir := t.TempDir()
	writeContent(t, seedDir, m, data)
	_, seedAddr := startSession(t, ctx, m, seedDir)

	sw := swarm.New(swarm.Config{MaxPeers: 10})
	sw.Add(seedAddr)
	got, err := FetchMetadata(ctx, sw, m.InfoHash(), id.NewPeerID(), MetadataConfig{})
	require.NoError(t, err)

	leech, _ := startSession(t, ctx, got, t.TempDir(), seedAddr)
	waitComplete(t, leech)
	cancel()
	// Sessions end their spans as Run returns.
	require.Eventually(t, func() bool {
		n := 0
		for _, s := range rec.Ended() {
			if s.Name() == "torrent.session" {
				n++
			}
		}
		return n == 2
	}, 5*time.Second, 10*time.Millisecond, "ended: %v", spanNames(rec))

	byName := map[string][]sdktrace.ReadOnlySpan{}
	for _, s := range rec.Ended() {
		byName[s.Name()] = append(byName[s.Name()], s)
	}

	fetch := byName["metadata.fetch"]
	require.Len(t, fetch, 1)
	require.Contains(t, fetch[0].Attributes(), attrString("torrent.name", m.Name()))
	require.NotEmpty(t, byName["metadata.attempt"])
	require.Equal(t, fetch[0].SpanContext().SpanID(), byName["metadata.attempt"][0].Parent().SpanID(), "attempts are children of the fetch")

	// Every peer connection is a child of a session span.
	sessions := map[[8]byte]bool{}
	for _, s := range byName["torrent.session"] {
		sessions[s.SpanContext().SpanID()] = true
	}
	require.NotEmpty(t, byName["peer.connection"])
	for _, p := range byName["peer.connection"] {
		require.True(t, sessions[p.Parent().SpanID()], "peer span parented by a session")
	}

	// The leecher's session records the download.
	var downloaded bool
	for _, s := range byName["torrent.session"] {
		for _, e := range s.Events() {
			if e.Name == "download complete" {
				downloaded = true
			}
		}
	}
	require.True(t, downloaded)
}

func attrString(k, v string) attribute.KeyValue { return attribute.String(k, v) }

func spanNames(rec *tracetest.SpanRecorder) map[string]int {
	out := map[string]int{}
	for _, s := range rec.Ended() {
		out[s.Name()]++
	}
	for _, s := range rec.Started() {
		out["started:"+s.Name()]++
	}
	return out
}
