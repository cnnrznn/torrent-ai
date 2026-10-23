// Package swarm tracks the peer addresses known for a torrent and caps
// how many are connected at once. Peer sources (trackers, the DHT) add
// addresses; the downloader takes them.
package swarm

import (
	"net/netip"
	"sync"
	"time"
)

// Config configures a Swarm. Zero fields take the defaults in
// DefaultConfig.
type Config struct {
	// MaxPeers caps active connections, incoming and outgoing.
	MaxPeers int
	// RetryBackoff is how long an address is skipped after its first
	// failure. It doubles with each further failure.
	RetryBackoff time.Duration
	// MaxBackoff caps how long a failing address is skipped.
	MaxBackoff time.Duration
}

// DefaultConfig returns the default swarm settings.
func DefaultConfig() Config {
	return Config{
		MaxPeers:     50,
		RetryBackoff: 30 * time.Second,
		MaxBackoff:   10 * time.Minute,
	}
}

type entry struct {
	active   bool
	failures int
	retryAt  time.Time
}

// Swarm is a set of peer addresses plus a connection cap. It is safe
// for concurrent use.
type Swarm struct {
	mu      sync.Mutex
	cfg     Config
	addrs   map[netip.AddrPort]*entry
	banned  map[netip.Addr]bool
	active  int
	notify  chan struct{}
	nowFunc func() time.Time
}

// New returns a swarm with the given settings.
func New(cfg Config) *Swarm {
	d := DefaultConfig()
	if cfg.MaxPeers <= 0 {
		cfg.MaxPeers = d.MaxPeers
	}
	if cfg.RetryBackoff <= 0 {
		cfg.RetryBackoff = d.RetryBackoff
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = d.MaxBackoff
	}

	return &Swarm{
		cfg:     cfg,
		addrs:   map[netip.AddrPort]*entry{},
		banned:  map[netip.Addr]bool{},
		notify:  make(chan struct{}, 1),
		nowFunc: time.Now,
	}
}

// Add records peer addresses. Known and invalid addresses are ignored.
func (s *Swarm) Add(addrs ...netip.AddrPort) {
	s.mu.Lock()
	added := false
	for _, a := range addrs {
		a = netip.AddrPortFrom(a.Addr().Unmap(), a.Port())
		if !a.IsValid() || a.Port() == 0 || a.Addr().IsUnspecified() {
			continue
		}
		if _, ok := s.addrs[a]; ok {
			continue
		}
		s.addrs[a] = &entry{}
		added = true
	}
	s.mu.Unlock()

	if added {
		select {
		case s.notify <- struct{}{}:
		default:
		}
	}
}

// Next reserves a connection slot for an address that isn't connected
// and isn't backing off after a failure. The caller must call Release
// when done with it.
func (s *Swarm) Next() (netip.AddrPort, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.active >= s.cfg.MaxPeers {
		return netip.AddrPort{}, false
	}

	now := s.nowFunc()
	for a, e := range s.addrs {
		if e.active || now.Before(e.retryAt) || s.banned[a.Addr()] {
			continue
		}
		e.active = true
		s.active++
		return a, true
	}

	return netip.AddrPort{}, false
}

// Acquire reserves a slot for an incoming connection from a. It fails
// if the swarm is full or a is already connected.
func (s *Swarm) Acquire(a netip.AddrPort) bool {
	a = netip.AddrPortFrom(a.Addr().Unmap(), a.Port())

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.active >= s.cfg.MaxPeers || s.banned[a.Addr()] {
		return false
	}
	e, ok := s.addrs[a]
	if !ok {
		e = &entry{}
		s.addrs[a] = e
	}
	if e.active {
		return false
	}
	e.active = true
	s.active++
	return true
}

// Release frees the slot held for a. If the connection failed, the
// address is skipped for a while, longer after each failure.
func (s *Swarm) Release(a netip.AddrPort, failed bool) {
	a = netip.AddrPortFrom(a.Addr().Unmap(), a.Port())

	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.addrs[a]
	if !ok || !e.active {
		return
	}
	e.active = false
	s.active--

	if failed {
		e.failures++
		backoff := s.cfg.MaxBackoff
		if shift := e.failures - 1; shift < 32 && s.cfg.RetryBackoff<<shift > 0 {
			backoff = min(s.cfg.RetryBackoff<<shift, s.cfg.MaxBackoff)
		}
		e.retryAt = s.nowFunc().Add(backoff)
	} else {
		e.failures = 0
		e.retryAt = time.Time{}
	}
}

// Ban stops handing out, or accepting connections from, any address
// with this IP. Slots already held are still returned with Release.
func (s *Swarm) Ban(ip netip.Addr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.banned[ip.Unmap()] = true
}

// Banned returns the number of banned IPs.
func (s *Swarm) Banned() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.banned)
}

// Notify returns a channel that receives a value after new addresses
// are added.
func (s *Swarm) Notify() <-chan struct{} {
	return s.notify
}

// Len returns the number of known addresses.
func (s *Swarm) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.addrs)
}

// Active returns the number of reserved connection slots.
func (s *Swarm) Active() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}
