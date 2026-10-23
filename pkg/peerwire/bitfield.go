package peerwire

import "fmt"

// Bitfield records which pieces a peer has. The high bit of the first
// byte is piece 0.
type Bitfield []byte

// NewBitfield returns an empty bitfield for n pieces.
func NewBitfield(n int) Bitfield {
	return make(Bitfield, (n+7)/8)
}

// ParseBitfield validates a bitfield received from a peer for a
// torrent with n pieces. The spare bits at the end must be zero.
func ParseBitfield(b []byte, n int) (Bitfield, error) {
	if len(b) != (n+7)/8 {
		return nil, fmt.Errorf("bitfield is %d bytes, want %d", len(b), (n+7)/8)
	}
	bf := Bitfield(append([]byte(nil), b...))
	for i := n; i < len(b)*8; i++ {
		if bf.Has(i) {
			return nil, fmt.Errorf("bitfield has spare bit %d set", i)
		}
	}
	return bf, nil
}

// Has reports whether piece i is set.
func (b Bitfield) Has(i int) bool {
	if i < 0 || i/8 >= len(b) {
		return false
	}
	return b[i/8]&(0x80>>(i%8)) != 0
}

// Set marks piece i as present.
func (b Bitfield) Set(i int) {
	if i < 0 || i/8 >= len(b) {
		return
	}
	b[i/8] |= 0x80 >> (i % 8)
}

// Count returns how many pieces are set.
func (b Bitfield) Count() int {
	n := 0
	for _, x := range b {
		for ; x != 0; x &= x - 1 {
			n++
		}
	}
	return n
}
