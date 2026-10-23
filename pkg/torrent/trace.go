package torrent

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// tracer is looked up on each use rather than stored, so spans follow
// whichever TracerProvider is installed globally, even one set later.
func tracer() trace.Tracer {
	return otel.Tracer("github.com/cnnrznn/torrent-ai/pkg/torrent")
}

// endSpan records err on span, unless it's a cancellation, and ends it.
func endSpan(span trace.Span, err error) {
	if err != nil && !errors.Is(err, context.Canceled) {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}
