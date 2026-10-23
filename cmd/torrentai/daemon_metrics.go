package main

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/cnnrznn/torrent-ai/pkg/daemon"
	"github.com/cnnrznn/torrent-ai/pkg/dht"
)

// registerDaemonMetrics exports every torrent's progress as OTel
// metrics, observed from Manager.List so they agree with the API. With
// perTorrent each series carries torrent.infohash and torrent.name;
// otherwise they are summed across torrents. DHT metrics are the same
// as the one-shot command's.
func registerDaemonMetrics(m *daemon.Manager, node *dht.Server, perTorrent bool) error {
	meter := otel.Meter(instrumentationName)

	type inst struct {
		name, unit, desc string
		counter          bool
		value            func(daemon.Status) int64
	}
	defs := []inst{
		{"torrent.pieces.verified", "{piece}", "Pieces downloaded and verified", false, func(s daemon.Status) int64 { return int64(s.Have) }},
		{"torrent.pieces.total", "{piece}", "Pieces in the torrent", false, func(s daemon.Status) int64 { return int64(s.Pieces) }},
		{"torrent.left", "By", "Bytes still to download", false, func(s daemon.Status) int64 { return s.Left }},
		{"torrent.peers.connected", "{peer}", "Connected peers", false, func(s daemon.Status) int64 { return int64(s.Peers) }},
		{"torrent.peers.unchoked_us", "{peer}", "Peers letting us request blocks", false, func(s daemon.Status) int64 { return int64(s.UnchokedUs) }},
		{"torrent.peers.interesting", "{peer}", "Peers that have pieces we need", false, func(s daemon.Status) int64 { return int64(s.Interested) }},
		{"torrent.requests.in_flight", "{request}", "Block requests awaiting a reply", false, func(s daemon.Status) int64 { return int64(s.Requests) }},
		{"torrent.buffered", "By", "Bytes held in pieces still being downloaded", false, func(s daemon.Status) int64 { return s.Buffered }},
		{"swarm.peers.known", "{peer}", "Peer addresses known from all sources", false, func(s daemon.Status) int64 { return int64(s.KnownPeers) }},
		{"swarm.peers.banned", "{ip}", "IPs banned for sending corrupt data", false, func(s daemon.Status) int64 { return int64(s.Banned) }},
		{"torrent.downloaded", "By", "Bytes of piece data downloaded", true, func(s daemon.Status) int64 { return s.Downloaded }},
		{"torrent.uploaded", "By", "Bytes of piece data uploaded", true, func(s daemon.Status) int64 { return s.Uploaded }},
		{"torrent.dials", "{dial}", "Outgoing peer connection attempts", true, func(s daemon.Status) int64 { return s.Dials }},
		{"torrent.dial_failures", "{dial}", "Outgoing peer connection attempts that failed", true, func(s daemon.Status) int64 { return s.DialFailures }},
		{"torrent.accepted", "{connection}", "Incoming peer connections accepted", true, func(s daemon.Status) int64 { return s.Accepted }},
		{"torrent.hash_failures", "{piece}", "Pieces that failed their SHA-1 check", true, func(s daemon.Status) int64 { return s.HashFailures }},
		{"torrent.request_timeouts", "{request}", "Block requests that went unanswered", true, func(s daemon.Status) int64 { return s.RequestTimeouts }},
	}

	observables := make([]metric.Observable, len(defs))
	for i, d := range defs {
		var err error
		if d.counter {
			observables[i], err = meter.Int64ObservableCounter(d.name, metric.WithUnit(d.unit), metric.WithDescription(d.desc))
		} else {
			observables[i], err = meter.Int64ObservableGauge(d.name, metric.WithUnit(d.unit), metric.WithDescription(d.desc))
		}
		if err != nil {
			return err
		}
	}
	torrents, err := meter.Int64ObservableGauge("daemon.torrents", metric.WithUnit("{torrent}"), metric.WithDescription("Torrents by state"))
	if err != nil {
		return err
	}

	observe := func(o metric.Observer, i int, v int64, opts ...metric.ObserveOption) {
		switch inst := observables[i].(type) {
		case metric.Int64ObservableCounter:
			o.ObserveInt64(inst, v, opts...)
		case metric.Int64ObservableGauge:
			o.ObserveInt64(inst, v, opts...)
		}
	}

	_, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		list := m.List()

		byState := map[daemon.State]int64{}
		for _, st := range list {
			byState[st.State]++
		}
		for _, state := range []daemon.State{daemon.StateMetadata, daemon.StateVerifying, daemon.StateDownloading, daemon.StateSeeding, daemon.StateFinished, daemon.StateFailed, daemon.StateStopped} {
			o.ObserveInt64(torrents, byState[state], metric.WithAttributes(attribute.String("torrent.state", string(state))))
		}

		if perTorrent {
			for _, st := range list {
				attrs := metric.WithAttributes(attribute.String("torrent.infohash", st.InfoHash), attribute.String("torrent.name", st.Name))
				for i, d := range defs {
					observe(o, i, d.value(st), attrs)
				}
			}
			return nil
		}
		for i, d := range defs {
			var sum int64
			for _, st := range list {
				sum += d.value(st)
			}
			observe(o, i, sum)
		}
		return nil
	}, append(observables, torrents)...)
	if err != nil {
		return err
	}

	if node != nil {
		return registerDHTMetrics(meter, node)
	}
	return nil
}
