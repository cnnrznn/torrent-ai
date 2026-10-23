package torrent

import "sync"

// MemoryBudget caps the bytes held in partial pieces, across one or
// more sessions. A nil budget is unlimited. It is safe for concurrent
// use.
type MemoryBudget struct {
	mu   sync.Mutex
	max  int64
	used int64
}

// NewMemoryBudget returns a budget of max bytes, or nil (unlimited) if
// max is zero or less.
func NewMemoryBudget(max int64) *MemoryBudget {
	if max <= 0 {
		return nil
	}
	return &MemoryBudget{max: max}
}

// reserve claims n bytes. force claims them even past the limit, so a
// session with nothing in progress can always make progress.
func (b *MemoryBudget) reserve(n int64, force bool) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !force && b.used+n > b.max {
		return false
	}
	b.used += n
	return true
}

func (b *MemoryBudget) release(n int64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used = max(0, b.used-n)
}

// Used returns the bytes currently claimed.
func (b *MemoryBudget) Used() int64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}
