package swarm

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var (
	a = netip.MustParseAddrPort("10.0.0.1:6881")
	b = netip.MustParseAddrPort("10.0.0.2:6881")
)

func TestSwarm(t *testing.T) {
	t.Run("hands out each address once", func(t *testing.T) {
		s := New(Config{MaxPeers: 10})
		s.Add(a, b, a)
		require.Equal(t, 2, s.Len())

		got := map[netip.AddrPort]bool{}
		for {
			addr, ok := s.Next()
			if !ok {
				break
			}
			got[addr] = true
		}
		require.Equal(t, map[netip.AddrPort]bool{a: true, b: true}, got)
		require.Equal(t, 2, s.Active())
	})

	t.Run("respects the limit", func(t *testing.T) {
		s := New(Config{MaxPeers: 1})
		s.Add(a, b)
		_, ok := s.Next()
		require.True(t, ok)
		_, ok = s.Next()
		require.False(t, ok)
		require.False(t, s.Acquire(netip.MustParseAddrPort("10.0.0.3:1")))
	})

	t.Run("backs off failed addresses", func(t *testing.T) {
		now := time.Now()
		s := New(Config{MaxPeers: 10})
		s.nowFunc = func() time.Time { return now }
		s.Add(a)

		addr, _ := s.Next()
		s.Release(addr, true)
		_, ok := s.Next()
		require.False(t, ok)

		now = now.Add(time.Minute)
		addr, ok = s.Next()
		require.True(t, ok)
		require.Equal(t, a, addr)
	})

	t.Run("reuses addresses released cleanly", func(t *testing.T) {
		s := New(Config{MaxPeers: 10})
		s.Add(a)
		addr, _ := s.Next()
		s.Release(addr, false)
		_, ok := s.Next()
		require.True(t, ok)
	})

	t.Run("rejects a duplicate incoming connection", func(t *testing.T) {
		s := New(Config{MaxPeers: 10})
		require.True(t, s.Acquire(a))
		require.False(t, s.Acquire(a))
		_, ok := s.Next()
		require.False(t, ok)
	})

	t.Run("notifies on new addresses", func(t *testing.T) {
		s := New(Config{MaxPeers: 10})
		s.Add(a)
		<-s.Notify()
		s.Add(a)
		select {
		case <-s.Notify():
			t.Fatal("notified for a known address")
		default:
		}
	})

	t.Run("ignores invalid addresses", func(t *testing.T) {
		s := New(Config{MaxPeers: 10})
		s.Add(netip.AddrPort{}, netip.MustParseAddrPort("10.0.0.1:0"), netip.MustParseAddrPort("0.0.0.0:1"))
		require.Zero(t, s.Len())
	})
}

func TestBan(t *testing.T) {
	s := New(Config{MaxPeers: 10})
	s.Add(a, b)
	s.Ban(a.Addr())
	require.Equal(t, 1, s.Banned())

	for {
		addr, ok := s.Next()
		if !ok {
			break
		}
		require.NotEqual(t, a.Addr(), addr.Addr())
	}
	require.False(t, s.Acquire(netip.AddrPortFrom(a.Addr(), 9999)), "banned by IP, any port")
}

func TestBackoffConfig(t *testing.T) {
	now := time.Now()
	s := New(Config{RetryBackoff: time.Second, MaxBackoff: 3 * time.Second})
	s.nowFunc = func() time.Time { return now }
	s.Add(a)

	// Backoff doubles per failure (1s, 2s) and is then capped at 3s.
	for _, want := range []time.Duration{time.Second, 2 * time.Second, 3 * time.Second, 3 * time.Second} {
		addr, ok := s.Next()
		require.True(t, ok)
		s.Release(addr, true)

		now = now.Add(want - time.Millisecond)
		_, ok = s.Next()
		require.False(t, ok, "still backing off before %v", want)
		now = now.Add(time.Millisecond)
	}
}

func TestSharedLimiter(t *testing.T) {
	l := NewLimiter(2)
	s1 := New(Config{MaxPeers: 10, Limiter: l})
	s2 := New(Config{MaxPeers: 10, Limiter: l})
	s1.Add(a, b)
	s2.Add(netip.MustParseAddrPort("10.0.0.3:1"), netip.MustParseAddrPort("10.0.0.4:1"))

	x, ok := s1.Next()
	require.True(t, ok)
	_, ok = s2.Next()
	require.True(t, ok)
	require.Equal(t, 2, l.Active())

	_, ok = s1.Next()
	require.False(t, ok, "the shared cap is reached")
	require.False(t, s2.Acquire(netip.MustParseAddrPort("10.0.0.9:9")))

	s1.Release(x, false)
	require.Equal(t, 1, l.Active())
	_, ok = s2.Next()
	require.True(t, ok, "a slot freed in one swarm is usable by another")
}
