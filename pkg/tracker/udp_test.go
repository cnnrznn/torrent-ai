package tracker

import (
	"bytes"
	"context"
	"encoding/binary"
	"log/slog"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/cnnrznn/torrent-ai/pkg/id"
)

// fakeUDPTracker answers BEP 15 connect and announce requests. It
// drops the first `drop` packets it receives.
type fakeUDPTracker struct {
	conn     *net.UDPConn
	drop     int
	announce chan []byte
	failWith string
}

func newFakeUDPTracker(t *testing.T) *fakeUDPTracker {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	return &fakeUDPTracker{conn: conn, announce: make(chan []byte, 10)}
}

func (f *fakeUDPTracker) url() string {
	return "udp://" + f.conn.LocalAddr().String() + "/announce"
}

func (f *fakeUDPTracker) serve() {
	const connID = 0xdeadbeef
	buf := make([]byte, 2048)
	for {
		n, addr, err := f.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if f.drop > 0 {
			f.drop--
			continue
		}

		req := buf[:n]
		tx := req[12:16]
		var res []byte
		switch {
		case f.failWith != "":
			res = binary.BigEndian.AppendUint32(nil, actionError)
			res = append(res, tx...)
			res = append(res, f.failWith...)
		case binary.BigEndian.Uint64(req) == udpProtocolID:
			res = binary.BigEndian.AppendUint32(nil, actionConnect)
			res = append(res, tx...)
			res = binary.BigEndian.AppendUint64(res, connID)
		case binary.BigEndian.Uint64(req) == connID:
			f.announce <- append([]byte(nil), req...)
			res = binary.BigEndian.AppendUint32(nil, actionAnnounce)
			res = append(res, tx...)
			res = binary.BigEndian.AppendUint32(res, 900) // interval
			res = binary.BigEndian.AppendUint32(res, 1)   // leechers
			res = binary.BigEndian.AppendUint32(res, 2)   // seeders
			res = append(res, 127, 0, 0, 1, 0x1a, 0xe1)
		default:
			continue
		}
		f.conn.WriteToUDP(res, addr)
	}
}

func TestUDPAnnounce(t *testing.T) {
	infoHash, peerID := id.Random(), id.NewPeerID()
	req := Request{InfoHash: infoHash, PeerID: peerID, Port: 6881, Left: 42, Event: EventStarted}

	t.Run("connects and announces", func(t *testing.T) {
		f := newFakeUDPTracker(t)
		go f.serve()

		tr, err := New(f.url())
		require.NoError(t, err)
		res, err := tr.Announce(context.Background(), req)
		require.NoError(t, err)

		require.Equal(t, 15*time.Minute, res.Interval)
		require.Equal(t, []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:6881")}, res.Peers)

		got := <-f.announce
		require.Len(t, got, 98)
		require.Equal(t, infoHash[:], got[16:36])
		require.Equal(t, peerID[:], got[36:56])
		require.EqualValues(t, 42, binary.BigEndian.Uint64(got[64:72]))
		require.EqualValues(t, 2, binary.BigEndian.Uint32(got[80:84]))
		require.EqualValues(t, 6881, binary.BigEndian.Uint16(got[96:98]))
	})

	t.Run("retries after a lost packet", func(t *testing.T) {
		f := newFakeUDPTracker(t)
		f.drop = 2
		go f.serve()

		tr, err := New(f.url(), WithUDPRetries(50*time.Millisecond, 3))
		require.NoError(t, err)
		res, err := tr.Announce(context.Background(), req)
		require.NoError(t, err)
		require.Len(t, res.Peers, 1)
	})

	t.Run("gives up after the last retry", func(t *testing.T) {
		f := newFakeUDPTracker(t)
		f.drop = 100
		go f.serve()

		tr, err := New(f.url(), WithUDPRetries(10*time.Millisecond, 2))
		require.NoError(t, err)
		_, err = tr.Announce(context.Background(), req)
		require.ErrorIs(t, err, errTimeout)
	})

	t.Run("stops when the context is cancelled", func(t *testing.T) {
		f := newFakeUDPTracker(t)
		f.drop = 100
		go f.serve()

		tr, err := New(f.url())
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err = tr.Announce(ctx, req)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Less(t, time.Since(start), 5*time.Second)
	})

	t.Run("returns the tracker's error message", func(t *testing.T) {
		f := newFakeUDPTracker(t)
		f.failWith = "unregistered torrent"
		go f.serve()

		tr, err := New(f.url())
		require.NoError(t, err)
		_, err = tr.Announce(context.Background(), req)
		require.ErrorContains(t, err, "unregistered torrent")
	})
}

func TestNewScheme(t *testing.T) {
	_, err := New("wss://tracker")
	require.ErrorContains(t, err, "unsupported tracker scheme")
	_, err = New("udp://tracker")
	require.ErrorContains(t, err, "no port")
}

func TestUDPLogsRetries(t *testing.T) {
	f := newFakeUDPTracker(t)
	f.drop = 1
	go f.serve()

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	tr, err := New(f.url(), WithUDPRetries(50*time.Millisecond, 3), WithLogger(log))
	require.NoError(t, err)
	_, err = tr.Announce(context.Background(), Request{InfoHash: id.Random(), PeerID: id.NewPeerID()})
	require.NoError(t, err)

	out := buf.String()
	require.Contains(t, out, `msg="udp tracker timeout"`)
	require.Contains(t, out, "step=connect")
	require.Contains(t, out, `msg="udp tracker response"`)
}
