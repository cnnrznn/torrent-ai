// Package id holds the 20-byte identifiers used throughout BitTorrent:
// infohashes, peer IDs and DHT node IDs.
package id

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// Hash is a 20-byte identifier.
type Hash [20]byte

// peerIDPrefix identifies this client in Azureus-style peer IDs.
const peerIDPrefix = "-TS0001-"

// FromString converts a raw 20-byte string into a Hash.
func FromString(s string) (Hash, error) {
	var h Hash
	if len(s) != len(h) {
		return h, fmt.Errorf("hash must be %d bytes, got %d", len(h), len(s))
	}
	copy(h[:], s)
	return h, nil
}

// FromHex parses a 40-character hex string into a Hash.
func FromHex(s string) (Hash, error) {
	bs, err := hex.DecodeString(s)
	if err != nil {
		return Hash{}, err
	}
	return FromString(string(bs))
}

// Random returns a random Hash, suitable for a DHT node ID.
func Random() Hash {
	var h Hash
	rand.Read(h[:])
	return h
}

// NewPeerID returns a random peer ID that starts with this client's
// prefix.
func NewPeerID() Hash {
	h := Random()
	copy(h[:], peerIDPrefix)
	return h
}

// String returns the hash as raw bytes, as sent on the wire.
func (h Hash) String() string {
	return string(h[:])
}

// Hex returns the hash in lowercase hex.
func (h Hash) Hex() string {
	return hex.EncodeToString(h[:])
}
