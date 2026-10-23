// Package peerwire implements the BitTorrent peer wire protocol
// (BEP 3) and the extension protocol (BEP 10).
package peerwire

import (
	"errors"
	"fmt"
	"io"

	"github.com/cnnrznn/torrent-ai/pkg/id"
)

const protocol = "BitTorrent protocol"

// HandshakeLen is the size of a handshake on the wire.
const HandshakeLen = 1 + len(protocol) + 8 + 20 + 20

// Handshake is the first message sent on a peer connection.
type Handshake struct {
	Reserved [8]byte
	InfoHash id.Hash
	PeerID   id.Hash
}

// NewHandshake returns a handshake that advertises support for the
// extension protocol and, if dht is set, the DHT.
func NewHandshake(infoHash, peerID id.Hash, dht bool) Handshake {
	h := Handshake{InfoHash: infoHash, PeerID: peerID}
	h.Reserved[5] |= 0x10
	if dht {
		h.Reserved[7] |= 0x01
	}
	return h
}

// SupportsExtensions reports whether the peer supports BEP 10.
func (h Handshake) SupportsExtensions() bool {
	return h.Reserved[5]&0x10 != 0
}

// SupportsDHT reports whether the peer runs a DHT node (BEP 5).
func (h Handshake) SupportsDHT() bool {
	return h.Reserved[7]&0x01 != 0
}

// Bytes encodes the handshake.
func (h Handshake) Bytes() []byte {
	b := make([]byte, 0, HandshakeLen)
	b = append(b, byte(len(protocol)))
	b = append(b, protocol...)
	b = append(b, h.Reserved[:]...)
	b = append(b, h.InfoHash[:]...)
	b = append(b, h.PeerID[:]...)
	return b
}

// ReadHandshake reads a handshake from r.
func ReadHandshake(r io.Reader) (Handshake, error) {
	var h Handshake

	b := make([]byte, HandshakeLen)
	if _, err := io.ReadFull(r, b[:1]); err != nil {
		return h, err
	}
	if int(b[0]) != len(protocol) {
		return h, fmt.Errorf("unexpected protocol length %d", b[0])
	}
	if _, err := io.ReadFull(r, b[1:]); err != nil {
		return h, err
	}

	b = b[1:]
	if string(b[:len(protocol)]) != protocol {
		return h, errors.New("unexpected protocol")
	}
	b = b[len(protocol):]

	copy(h.Reserved[:], b[:8])
	copy(h.InfoHash[:], b[8:28])
	copy(h.PeerID[:], b[28:48])

	return h, nil
}

// ClientName guesses the peer's client from its peer ID. Azureus-style
// IDs ("-qB4630-...") give the client code and version; otherwise the
// printable prefix is returned.
func ClientName(peerID id.Hash) string {
	if peerID[0] == '-' && peerID[7] == '-' {
		return string(peerID[1:7])
	}
	b := make([]byte, 0, 8)
	for _, c := range peerID[:8] {
		if c < 0x20 || c > 0x7e {
			c = '.'
		}
		b = append(b, c)
	}
	return string(b)
}
