package peerwire

import (
	"fmt"

	"github.com/cnnrznn/torrent-scratch/pkg/bencoding"
)

// ExtHandshakeID is the extended message ID of the BEP 10 handshake.
const ExtHandshakeID = 0

// ExtHandshake is the BEP 10 extension handshake.
type ExtHandshake struct {
	// M maps extension names to the message IDs the sender wants to
	// receive them on.
	M map[string]int
	// MetadataSize is the size of the info dict in bytes, if known
	// (BEP 9).
	MetadataSize int
	// V is the client name and version.
	V string
}

// Encode bencodes the handshake.
func (h ExtHandshake) Encode() []byte {
	m := map[string]string{}
	for name, id := range h.M {
		m[name] = bencoding.EncodeInteger(id)
	}

	d := map[string]string{"m": bencoding.EncodeDictionary(m)}
	if h.MetadataSize > 0 {
		d["metadata_size"] = bencoding.EncodeInteger(h.MetadataSize)
	}
	if h.V != "" {
		d["v"] = bencoding.EncodeString(h.V)
	}

	return []byte(bencoding.EncodeDictionary(d))
}

// ParseExtHandshake decodes an extension handshake. Unknown keys and
// malformed optional fields are ignored.
func ParseExtHandshake(b []byte) (ExtHandshake, error) {
	h := ExtHandshake{M: map[string]int{}}

	d, _, err := bencoding.ParseDictionary(string(b))
	if err != nil {
		return h, fmt.Errorf("invalid extension handshake: %w", err)
	}

	if raw, ok := d["m"]; ok {
		m, _, err := bencoding.ParseDictionary(raw)
		if err != nil {
			return h, fmt.Errorf("invalid extension map: %w", err)
		}
		for name, raw := range m {
			if id, _, err := bencoding.ParseInteger(raw); err == nil && id > 0 && id < 256 {
				h.M[name] = id
			}
		}
	}

	if raw, ok := d["metadata_size"]; ok {
		h.MetadataSize, _, _ = bencoding.ParseInteger(raw)
	}
	if raw, ok := d["v"]; ok {
		h.V, _, _ = bencoding.ParseString(raw)
	}

	return h, nil
}
