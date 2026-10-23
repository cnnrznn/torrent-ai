package torrent

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/cnnrznn/torrent-ai/pkg/peerwire"
)

func full(n int) peerwire.Bitfield {
	bf := peerwire.NewBitfield(n)
	for i := range n {
		bf.Set(i)
	}
	return bf
}

func never(block) bool { return false }

func TestPicker(t *testing.T) {
	const pieceLength = 2 * peerwire.BlockSize

	t.Run("splits pieces into blocks", func(t *testing.T) {
		// 3 pieces: two full ones and a short last one.
		m, _ := makeTorrent(t, pieceLength, 2*pieceLength+100)
		p := newPicker(m, peerwire.NewBitfield(3), peerwire.BlockSize)
		require.Equal(t, 2, p.numBlocks(0))
		require.Equal(t, 1, p.numBlocks(2))
		require.Equal(t, block{2, 0, 100}, p.block(2, 0))
		require.Equal(t, block{0, peerwire.BlockSize, peerwire.BlockSize}, p.block(0, 1))
	})

	t.Run("prefers the rarest piece", func(t *testing.T) {
		m, _ := makeTorrent(t, pieceLength, 3*pieceLength)
		p := newPicker(m, peerwire.NewBitfield(3), peerwire.BlockSize)
		for _, i := range []int{0, 0, 1, 2, 2} {
			p.peerHas(i)
		}

		b, ok := p.next(full(3), never)
		require.True(t, ok)
		require.Equal(t, 1, b.piece)
	})

	t.Run("finishes started pieces first", func(t *testing.T) {
		m, _ := makeTorrent(t, pieceLength, 3*pieceLength)
		p := newPicker(m, peerwire.NewBitfield(3), peerwire.BlockSize)

		b1, _ := p.next(full(3), never)
		p.requested(b1)
		b2, _ := p.next(full(3), never)
		p.requested(b2)
		require.Equal(t, b1.piece, b2.piece)
		require.NotEqual(t, b1.begin, b2.begin)
	})

	t.Run("only picks pieces the peer has", func(t *testing.T) {
		m, _ := makeTorrent(t, pieceLength, 3*pieceLength)
		p := newPicker(m, peerwire.NewBitfield(3), peerwire.BlockSize)

		bf := peerwire.NewBitfield(3)
		bf.Set(2)
		b, ok := p.next(bf, never)
		require.True(t, ok)
		require.Equal(t, 2, b.piece)

		_, ok = p.next(peerwire.NewBitfield(3), never)
		require.False(t, ok)
	})

	t.Run("duplicates outstanding blocks in endgame", func(t *testing.T) {
		m, _ := makeTorrent(t, pieceLength, peerwire.BlockSize)
		p := newPicker(m, peerwire.NewBitfield(1), peerwire.BlockSize)

		b, ok := p.next(full(1), never)
		require.True(t, ok)
		p.requested(b)

		sent := func(x block) bool { return x == b }
		_, ok = p.next(full(1), sent)
		require.False(t, ok, "same peer must not repeat a request")

		dup, ok := p.next(full(1), never)
		require.True(t, ok)
		require.Equal(t, b, dup)
	})

	t.Run("re-requests released blocks", func(t *testing.T) {
		m, _ := makeTorrent(t, pieceLength, peerwire.BlockSize)
		p := newPicker(m, peerwire.NewBitfield(1), peerwire.BlockSize)

		b, _ := p.next(full(1), never)
		p.requested(b)
		p.released(b)

		again, ok := p.next(full(1), func(block) bool { return true })
		require.True(t, ok)
		require.Equal(t, b, again)
	})

	t.Run("completes and verifies pieces", func(t *testing.T) {
		m, data := makeTorrent(t, pieceLength, pieceLength)
		p := newPicker(m, peerwire.NewBitfield(1), peerwire.BlockSize)

		b0, _ := p.next(full(1), never)
		p.requested(b0)
		b1, _ := p.next(full(1), never)
		p.requested(b1)

		isNew, complete := p.received(b0, data[b0.begin:b0.begin+b0.length])
		require.True(t, isNew)
		require.False(t, complete)

		isNew, _ = p.received(b0, data[b0.begin:b0.begin+b0.length])
		require.False(t, isNew, "duplicate block")

		_, complete = p.received(b1, data[b1.begin:b1.begin+b1.length])
		require.True(t, complete)
		require.Equal(t, data, p.pieceData(0))

		p.finish(0, true)
		require.True(t, p.complete())
		_, ok := p.next(full(1), never)
		require.False(t, ok)
	})

	t.Run("downloads a piece again after a failed hash", func(t *testing.T) {
		m, _ := makeTorrent(t, pieceLength, peerwire.BlockSize)
		p := newPicker(m, peerwire.NewBitfield(1), peerwire.BlockSize)

		b, _ := p.next(full(1), never)
		p.requested(b)
		_, complete := p.received(b, make([]byte, b.length))
		require.True(t, complete)

		p.finish(0, false)
		require.False(t, p.complete())
		again, ok := p.next(full(1), never)
		require.True(t, ok)
		require.Equal(t, b, again)
	})
}

func TestPickerMemoryCap(t *testing.T) {
	const pieceLength = 2 * peerwire.BlockSize
	m, data := makeTorrent(t, pieceLength, 3*pieceLength)

	p := newPicker(m, peerwire.NewBitfield(3), peerwire.BlockSize)
	p.maxBuffered = pieceLength

	// Peer A has only piece 0, peer B only piece 1.
	a, b := peerwire.NewBitfield(3), peerwire.NewBitfield(3)
	a.Set(0)
	b.Set(1)

	blk, ok := p.next(a, never)
	require.True(t, ok)
	require.EqualValues(t, pieceLength, p.buffered)

	_, ok = p.next(b, never)
	require.False(t, ok, "a second piece would exceed the cap")

	// Finishing piece 0 frees its memory.
	p.requested(blk)
	p.received(blk, data[blk.begin:blk.begin+blk.length])
	blk2, _ := p.next(a, never)
	p.requested(blk2)
	_, complete := p.received(blk2, data[blk2.begin:blk2.begin+blk2.length])
	require.True(t, complete)
	p.finish(0, true)
	require.Zero(t, p.buffered)

	_, ok = p.next(b, never)
	require.True(t, ok)
}

func TestPickerMemoryCapSmallerThanAPiece(t *testing.T) {
	const pieceLength = 2 * peerwire.BlockSize
	m, _ := makeTorrent(t, pieceLength, 2*pieceLength)
	p := newPicker(m, peerwire.NewBitfield(2), peerwire.BlockSize)
	p.maxBuffered = 1

	_, ok := p.next(full(2), never)
	require.True(t, ok, "one piece may always be in progress")
}
