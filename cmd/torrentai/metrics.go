package main

import (
	"context"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"github.com/cnnrznn/torrent-ai/pkg/dht"
	"github.com/cnnrznn/torrent-ai/pkg/swarm"
	"github.com/cnnrznn/torrent-ai/pkg/torrent"
)

// registerMetrics exports the session, swarm and DHT counters as OTel
// metrics. They're observed from the same Stats snapshots the status
// lines print, so the two always agree. session is nil until the
// metadata is known; node is nil without the DHT.
func registerMetrics(sess *atomic.Pointer[torrent.Session], sw *swarm.Swarm, node *dht.Server) error {
	m := otel.Meter(instrumentationName)

	type gauge struct {
		name, unit, desc string
		value            func(torrent.Stats) int64
	}
	sessionGauges := []gauge{
		{"torrent.pieces.verified", "{piece}", "Pieces downloaded and verified", func(s torrent.Stats) int64 { return int64(s.Have) }},
		{"torrent.pieces.total", "{piece}", "Pieces in the torrent", func(s torrent.Stats) int64 { return int64(s.Pieces) }},
		{"torrent.left", "By", "Bytes still to download", func(s torrent.Stats) int64 { return s.Left }},
		{"torrent.peers.connected", "{peer}", "Connected peers", func(s torrent.Stats) int64 { return int64(s.Peers) }},
		{"torrent.peers.unchoked_us", "{peer}", "Peers letting us request blocks", func(s torrent.Stats) int64 { return int64(s.UnchokedUs) }},
		{"torrent.peers.interesting", "{peer}", "Peers that have pieces we need", func(s torrent.Stats) int64 { return int64(s.Interested) }},
		{"torrent.requests.in_flight", "{request}", "Block requests awaiting a reply", func(s torrent.Stats) int64 { return int64(s.Requests) }},
		{"torrent.buffered", "By", "Bytes held in pieces still being downloaded", func(s torrent.Stats) int64 { return s.Buffered }},
	}
	sessionCounters := []gauge{
		{"torrent.downloaded", "By", "Bytes of piece data downloaded", func(s torrent.Stats) int64 { return s.Downloaded }},
		{"torrent.uploaded", "By", "Bytes of piece data uploaded", func(s torrent.Stats) int64 { return s.Uploaded }},
		{"torrent.dials", "{dial}", "Outgoing peer connection attempts", func(s torrent.Stats) int64 { return s.Dials }},
		{"torrent.dial_failures", "{dial}", "Outgoing peer connection attempts that failed", func(s torrent.Stats) int64 { return s.DialFailures }},
		{"torrent.accepted", "{connection}", "Incoming peer connections accepted", func(s torrent.Stats) int64 { return s.Accepted }},
		{"torrent.hash_failures", "{piece}", "Pieces that failed their SHA-1 check", func(s torrent.Stats) int64 { return s.HashFailures }},
		{"torrent.request_timeouts", "{request}", "Block requests that went unanswered", func(s torrent.Stats) int64 { return s.RequestTimeouts }},
	}

	var observables []metric.Observable
	sessionGaugeInst := make([]metric.Int64ObservableGauge, len(sessionGauges))
	for i, g := range sessionGauges {
		inst, err := m.Int64ObservableGauge(g.name, metric.WithUnit(g.unit), metric.WithDescription(g.desc))
		if err != nil {
			return err
		}
		sessionGaugeInst[i] = inst
		observables = append(observables, inst)
	}
	sessionCounterInst := make([]metric.Int64ObservableCounter, len(sessionCounters))
	for i, g := range sessionCounters {
		inst, err := m.Int64ObservableCounter(g.name, metric.WithUnit(g.unit), metric.WithDescription(g.desc))
		if err != nil {
			return err
		}
		sessionCounterInst[i] = inst
		observables = append(observables, inst)
	}

	known, err := m.Int64ObservableGauge("swarm.peers.known", metric.WithUnit("{peer}"), metric.WithDescription("Peer addresses known from all sources"))
	if err != nil {
		return err
	}
	banned, err := m.Int64ObservableGauge("swarm.peers.banned", metric.WithUnit("{ip}"), metric.WithDescription("IPs banned for sending corrupt data"))
	if err != nil {
		return err
	}
	observables = append(observables, known, banned)

	_, err = m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveInt64(known, int64(sw.Len()))
		o.ObserveInt64(banned, int64(sw.Banned()))

		if s := sess.Load(); s != nil {
			st := s.Stats()
			for i, g := range sessionGauges {
				o.ObserveInt64(sessionGaugeInst[i], g.value(st))
			}
			for i, g := range sessionCounters {
				o.ObserveInt64(sessionCounterInst[i], g.value(st))
			}
		}
		return nil
	}, observables...)
	if err != nil || node == nil {
		return err
	}
	return registerDHTMetrics(m, node)
}

// registerDHTMetrics exports the DHT node's counters.
func registerDHTMetrics(m metric.Meter, node *dht.Server) error {
	type dhtMetric struct {
		name, unit, desc string
		counter          bool
		value            func(dht.Stats) int64
	}
	dhtMetrics := []dhtMetric{
		{"dht.nodes", "{node}", "Nodes in the routing table", false, func(s dht.Stats) int64 { return int64(s.Nodes) }},
		{"dht.buckets", "{bucket}", "Routing table buckets", false, func(s dht.Stats) int64 { return int64(s.Buckets) }},
		{"dht.stored_peers", "{peer}", "Peers other nodes announced to us", false, func(s dht.Stats) int64 { return int64(s.StoredPeers) }},
		{"dht.queries.sent", "{query}", "Queries we sent", true, func(s dht.Stats) int64 { return s.QueriesSent }},
		{"dht.replies", "{reply}", "Replies to our queries", true, func(s dht.Stats) int64 { return s.Replies }},
		{"dht.error_replies", "{reply}", "KRPC errors in reply to our queries", true, func(s dht.Stats) int64 { return s.ErrorReplies }},
		{"dht.timeouts", "{query}", "Our queries that went unanswered", true, func(s dht.Stats) int64 { return s.Timeouts }},
		{"dht.queries.received", "{query}", "Queries other nodes sent us", true, func(s dht.Stats) int64 { return s.QueriesReceived }},
		{"dht.announces.received", "{announce}", "announce_peer queries we accepted", true, func(s dht.Stats) int64 { return s.Announced }},
		{"dht.bad_packets", "{packet}", "Packets we couldn't decode", true, func(s dht.Stats) int64 { return s.BadPackets }},
	}
	insts := make([]metric.Observable, len(dhtMetrics))
	for i, d := range dhtMetrics {
		var err error
		if d.counter {
			insts[i], err = m.Int64ObservableCounter(d.name, metric.WithUnit(d.unit), metric.WithDescription(d.desc))
		} else {
			insts[i], err = m.Int64ObservableGauge(d.name, metric.WithUnit(d.unit), metric.WithDescription(d.desc))
		}
		if err != nil {
			return err
		}
	}
	_, err := m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		st := node.Stats()
		for i, d := range dhtMetrics {
			switch inst := insts[i].(type) {
			case metric.Int64ObservableCounter:
				o.ObserveInt64(inst, d.value(st))
			case metric.Int64ObservableGauge:
				o.ObserveInt64(inst, d.value(st))
			}
		}
		return nil
	}, insts...)
	return err
}
