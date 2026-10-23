package tracker

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// tracer is looked up on each use rather than stored, so spans follow
// whichever TracerProvider is installed globally, even one set later.
func tracer() trace.Tracer {
	return otel.Tracer("github.com/cnnrznn/torrent-ai/pkg/tracker")
}

// tracedTracker wraps a Tracker so each announce is a span.
type tracedTracker struct {
	Tracker
	url      string // redacted
	protocol string
}

func traced(t Tracker, announce, protocol string) Tracker {
	return &tracedTracker{Tracker: t, url: Redact(announce), protocol: protocol}
}

func (t *tracedTracker) Announce(ctx context.Context, r Request) (*Response, error) {
	ctx, span := tracer().Start(ctx, "tracker.announce", trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(
		attribute.String("tracker.url", t.url),
		attribute.String("tracker.protocol", t.protocol),
		attribute.String("tracker.event", string(r.Event)),
		attribute.String("torrent.infohash", r.InfoHash.Hex()),
	))
	defer span.End()

	res, err := t.Tracker.Announce(ctx, r)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		return nil, err
	}
	span.SetAttributes(
		attribute.Int("tracker.peers", len(res.Peers)),
		attribute.Int64("tracker.interval_s", int64(res.Interval.Seconds())),
	)
	return res, nil
}
