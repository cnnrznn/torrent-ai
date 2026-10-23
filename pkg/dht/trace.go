package dht

import (
	"context"
	"errors"
	"net/netip"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/cnnrznn/torrent-ai/pkg/id"
)

// tracer is looked up on each use rather than stored, so spans follow
// whichever TracerProvider is installed globally, even one set later.
func tracer() trace.Tracer {
	return otel.Tracer("github.com/cnnrznn/torrent-ai/pkg/dht")
}

// Bootstrap joins the DHT through the given router addresses
// ("host:port"), then looks up our own ID to fill the routing table.
func (s *Server) Bootstrap(ctx context.Context, routers []string) (LookupStats, error) {
	ctx, span := tracer().Start(ctx, "dht.bootstrap", trace.WithAttributes(attribute.Int("dht.routers", len(routers))))
	st, err := s.bootstrapLookup(ctx, routers)
	s.endLookup(span, st, err)
	return st, err
}

// GetPeers looks up peers for infoHash, calling found with each batch
// as it arrives.
func (s *Server) GetPeers(ctx context.Context, infoHash id.Hash, found func([]netip.AddrPort)) (LookupStats, error) {
	ctx, span := tracer().Start(ctx, "dht.get_peers", trace.WithAttributes(attribute.String("torrent.infohash", infoHash.Hex())))
	st, err := s.peersLookup(ctx, infoHash, found)
	s.endLookup(span, st, err)
	return st, err
}

// Announce looks up peers for infoHash like GetPeers, then announces
// that we accept peer connections on port to the closest nodes that
// gave us a token.
func (s *Server) Announce(ctx context.Context, infoHash id.Hash, port int, found func([]netip.AddrPort)) (LookupStats, error) {
	ctx, span := tracer().Start(ctx, "dht.announce", trace.WithAttributes(
		attribute.String("torrent.infohash", infoHash.Hex()),
		attribute.Int("dht.port", port),
	))
	st, err := s.announceLookup(ctx, infoHash, port, found)
	s.endLookup(span, st, err)
	return st, err
}

// endLookup records a lookup's stats on its span and ends it.
func (s *Server) endLookup(span trace.Span, st LookupStats, err error) {
	span.SetAttributes(
		attribute.Int("dht.lookup.queried", st.Queried),
		attribute.Int("dht.lookup.responded", st.Responded),
		attribute.Int("dht.lookup.failed", st.Failed),
		attribute.Int("dht.lookup.peers", st.Peers),
		attribute.Int("dht.lookup.announced", st.Announced),
		attribute.Int("dht.lookup.closest_bits", st.ClosestBits),
		attribute.Int("dht.nodes", s.table.len()),
	)
	if err != nil && !errors.Is(err, context.Canceled) {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}
