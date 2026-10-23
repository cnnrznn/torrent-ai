package torrent

import (
	"math/rand/v2"

	"github.com/cnnrznn/torrent-ai/pkg/metainfo"
	"github.com/cnnrznn/torrent-ai/pkg/peerwire"
)

// block is a request-sized chunk of a piece.
type block struct {
	piece, begin, length int
}

// partialPiece is a piece that has been started but not verified.
type partialPiece struct {
	data      []byte
	reqs      []int  // outstanding requests per block
	got       []bool // received blocks
	remaining int
}

// picker decides which blocks to request. It finishes partial pieces
// before starting new ones, starts the rarest pieces first, and once a
// peer has nothing unrequested left to offer, lets it duplicate
// requests already sent to other peers (endgame).
//
// picker is not safe for concurrent use; the session goroutine owns it.
type picker struct {
	m         *metainfo.Metainfo
	blockSize int

	// budget caps the bytes held in partial pieces, possibly shared
	// with other sessions; nil means no limit. One piece may always be
	// in progress, so a cap smaller than a piece slows the download
	// instead of stalling it.
	budget   *MemoryBudget
	buffered int64 // this picker's share
	have        peerwire.Bitfield
	avail       []int
	partial     map[int]*partialPiece
}

func newPicker(m *metainfo.Metainfo, have peerwire.Bitfield, blockSize int) *picker {
	return &picker{
		m:         m,
		blockSize: blockSize,
		have:      have,
		avail:     make([]int, m.NumPieces()),
		partial:   map[int]*partialPiece{},
	}
}

func (p *picker) numBlocks(piece int) int {
	return (p.m.PieceSize(piece) + p.blockSize - 1) / p.blockSize
}

func (p *picker) block(piece, i int) block {
	begin := i * p.blockSize
	return block{
		piece:  piece,
		begin:  begin,
		length: min(p.blockSize, p.m.PieceSize(piece)-begin),
	}
}

// peerHas records that a peer has piece i.
func (p *picker) peerHas(i int) {
	p.avail[i]++
}

// peerLeft removes a departing peer's pieces from the availability
// counts.
func (p *picker) peerLeft(bf peerwire.Bitfield) {
	for i := range p.avail {
		if bf.Has(i) {
			p.avail[i]--
		}
	}
}

// wants reports whether a peer with bitfield bf has anything we need.
func (p *picker) wants(bf peerwire.Bitfield) bool {
	for i := range p.avail {
		if bf.Has(i) && !p.have.Has(i) {
			return true
		}
	}
	return false
}

// next picks a block to request from a peer with bitfield bf.
// sentByPeer reports whether that peer already has a request out for
// a block, so endgame doesn't ask the same peer twice.
func (p *picker) next(bf peerwire.Bitfield, sentByPeer func(block) bool) (block, bool) {
	// Finish what we've started.
	for i, pp := range p.partial {
		if !bf.Has(i) {
			continue
		}
		for j := range pp.got {
			if !pp.got[j] && pp.reqs[j] == 0 {
				return p.block(i, j), true
			}
		}
	}

	// Start the rarest piece this peer has. Begin the scan at a random
	// point so peers don't all pile onto the same piece on ties.
	n := len(p.avail)
	best := -1
	if n > 0 {
		start := rand.IntN(n)
		for k := range n {
			i := (start + k) % n
			if p.have.Has(i) || !bf.Has(i) || p.partial[i] != nil {
				continue
			}
			if best == -1 || p.avail[i] < p.avail[best] {
				best = i
			}
		}
	}
	if best != -1 && !p.budget.reserve(int64(p.m.PieceSize(best)), len(p.partial) == 0) {
		best = -1
	}
	if best != -1 {
		nb := p.numBlocks(best)
		p.buffered += int64(p.m.PieceSize(best))
		p.partial[best] = &partialPiece{
			data:      make([]byte, p.m.PieceSize(best)),
			reqs:      make([]int, nb),
			got:       make([]bool, nb),
			remaining: nb,
		}
		return p.block(best, 0), true
	}

	// Endgame: double up on blocks still outstanding elsewhere.
	for i, pp := range p.partial {
		if !bf.Has(i) {
			continue
		}
		for j := range pp.got {
			if b := p.block(i, j); !pp.got[j] && !sentByPeer(b) {
				return b, true
			}
		}
	}

	return block{}, false
}

// requested records that a request for b was sent.
func (p *picker) requested(b block) {
	if pp := p.partial[b.piece]; pp != nil {
		pp.reqs[b.begin/p.blockSize]++
	}
}

// released records that a request for b won't be answered (cancelled,
// choked, timed out or disconnected).
func (p *picker) released(b block) {
	if pp := p.partial[b.piece]; pp != nil {
		j := b.begin / p.blockSize
		if pp.reqs[j] > 0 {
			pp.reqs[j]--
		}
	}
}

// received stores a block's data. It reports whether the block was new
// and whether its piece is now complete and ready to verify.
func (p *picker) received(b block, data []byte) (isNew, complete bool) {
	pp := p.partial[b.piece]
	if pp == nil {
		return false, false
	}

	j := b.begin / p.blockSize
	if pp.reqs[j] > 0 {
		pp.reqs[j]--
	}
	if pp.got[j] {
		return false, false
	}

	copy(pp.data[b.begin:], data)
	pp.got[j] = true
	pp.remaining--
	return true, pp.remaining == 0
}

// pieceData returns the assembled data of a complete partial piece.
func (p *picker) pieceData(i int) []byte {
	return p.partial[i].data
}

// finish marks piece i as verified, or throws its data away if it
// failed the hash check so it is downloaded again.
func (p *picker) finish(i int, ok bool) {
	pp := p.partial[i]
	delete(p.partial, i)
	if ok {
		if pp != nil {
			p.buffered -= int64(len(pp.data))
			p.budget.release(int64(len(pp.data)))
		}
		p.have.Set(i)
		return
	}
	if pp != nil {
		// Keep outstanding request counts so late replies are matched.
		reqs := pp.reqs
		nb := len(reqs)
		p.partial[i] = &partialPiece{
			data:      make([]byte, len(pp.data)),
			reqs:      reqs,
			got:       make([]bool, nb),
			remaining: nb,
		}
	}
}

// releaseAll returns the memory of every partial piece to the budget,
// for when the session stops.
func (p *picker) releaseAll() {
	for i, pp := range p.partial {
		p.budget.release(int64(len(pp.data)))
		p.buffered -= int64(len(pp.data))
		delete(p.partial, i)
	}
}

// complete reports whether every piece is verified.
func (p *picker) complete() bool {
	return p.have.Count() == p.m.NumPieces()
}
