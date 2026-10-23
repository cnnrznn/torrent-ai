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
		s := New(10)
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
		s := New(1)
		s.Add(a, b)
		_, ok := s.Next()
		require.True(t, ok)
		_, ok = s.Next()
		require.False(t, ok)
		require.False(t, s.Acquire(netip.MustParseAddrPort("10.0.0.3:1")))
	})

	t.Run("backs off failed addresses", func(t *testing.T) {
		now := time.Now()
		s := New(10)
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
		s := New(10)
		s.Add(a)
		addr, _ := s.Next()
		s.Release(addr, false)
		_, ok := s.Next()
		require.True(t, ok)
	})

	t.Run("rejects a duplicate incoming connection", func(t *testing.T) {
		s := New(10)
		require.True(t, s.Acquire(a))
		require.False(t, s.Acquire(a))
		_, ok := s.Next()
		require.False(t, ok)
	})

	t.Run("notifies on new addresses", func(t *testing.T) {
		s := New(10)
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
		s := New(10)
		s.Add(netip.AddrPort{}, netip.MustParseAddrPort("10.0.0.1:0"), netip.MustParseAddrPort("0.0.0.0:1"))
		require.Zero(t, s.Len())
	})
}

func TestBan(t *testing.T) {
	s := New(10)
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
