package peerwire

import (
	"context"
	"crypto/sha1"
	"errors"
	"fmt"

	"github.com/cnnrznn/torrent-ai/pkg/bencoding"
	"github.com/cnnrznn/torrent-ai/pkg/id"
)

// BEP 9 metadata exchange.
const (
	// UTMetadata is the extension name for metadata exchange.
	UTMetadata = "ut_metadata"
	// UTMetadataID is the extended message ID we ask peers to use when
	// sending us ut_metadata messages.
	UTMetadataID = 1

	// MetadataPieceSize is the spec-defined size of metadata pieces
	// (BEP 9: 16 KiB, except the last).
	MetadataPieceSize = 16 * 1024
	// MaxMetadataSize is the default bound on the info dict we'll
	// accept from a peer.
	MaxMetadataSize = 16 << 20
)

// MetadataOptions tunes metadata exchange. Zero fields take the
// defaults in DefaultMetadataOptions.
type MetadataOptions struct {
	// PieceSize is the size of metadata pieces. Peers assume 16 KiB
	// (BEP 9); other values only work between peers that agree.
	PieceSize int
	// MaxSize is the largest info dict we'll accept.
	MaxSize int
	// Window is how many piece requests are kept in flight.
	Window int
}

// DefaultMetadataOptions returns the default metadata settings.
func DefaultMetadataOptions() MetadataOptions {
	return MetadataOptions{
		PieceSize: MetadataPieceSize,
		MaxSize:   MaxMetadataSize,
		Window:    16,
	}
}

func (o MetadataOptions) withDefaults() MetadataOptions {
	d := DefaultMetadataOptions()
	if o.PieceSize <= 0 {
		o.PieceSize = d.PieceSize
	}
	if o.MaxSize <= 0 {
		o.MaxSize = d.MaxSize
	}
	if o.Window <= 0 {
		o.Window = d.Window
	}
	return o
}

// Reasons FetchMetadata can fail because of the peer.
var (
	ErrNoExtensions     = errors.New("peer does not support extensions")
	ErrNoUTMetadata     = errors.New("peer does not support ut_metadata")
	ErrMetadataRejected = errors.New("peer rejected metadata request")
	ErrMetadataHash     = errors.New("metadata does not match infohash")
)

const (
	MetadataRequest = 0
	MetadataData    = 1
	MetadataReject  = 2
)

// MetadataMsg is a ut_metadata message.
type MetadataMsg struct {
	Type      int
	Piece     int
	TotalSize int    // data messages only
	Data      []byte // data messages only
}

// Encode returns the message's bencoded dict, followed by the raw piece
// data for a data message.
func (m MetadataMsg) Encode() []byte {
	d := map[string]string{
		"msg_type": bencoding.EncodeInteger(m.Type),
		"piece":    bencoding.EncodeInteger(m.Piece),
	}
	if m.Type == MetadataData {
		d["total_size"] = bencoding.EncodeInteger(m.TotalSize)
	}
	return append([]byte(bencoding.EncodeDictionary(d)), m.Data...)
}

// ParseMetadataMsg decodes a ut_metadata message.
func ParseMetadataMsg(b []byte) (MetadataMsg, error) {
	var m MetadataMsg

	d, size, err := bencoding.ParseDictionary(string(b))
	if err != nil {
		return m, fmt.Errorf("invalid metadata message: %w", err)
	}
	if m.Type, _, err = bencoding.ParseInteger(d["msg_type"]); err != nil {
		return m, fmt.Errorf("invalid msg_type: %w", err)
	}
	if m.Piece, _, err = bencoding.ParseInteger(d["piece"]); err != nil {
		return m, fmt.Errorf("invalid piece: %w", err)
	}
	if m.Type == MetadataData {
		if m.TotalSize, _, err = bencoding.ParseInteger(d["total_size"]); err != nil {
			return m, fmt.Errorf("invalid total_size: %w", err)
		}
		m.Data = b[size:]
	}

	return m, nil
}

// FetchMetadata downloads the info dict for infoHash from the peer on
// c and checks it against the hash. It must be the only reader of c's
// messages while it runs. It sends our extension handshake itself.
func FetchMetadata(ctx context.Context, c *Conn, infoHash id.Hash, opts MetadataOptions) (string, error) {
	opts = opts.withDefaults()
	if !c.Peer.SupportsExtensions() {
		return "", ErrNoExtensions
	}

	hs := ExtHandshake{M: map[string]int{UTMetadata: UTMetadataID}}
	if err := c.Send(MsgExtended(ExtHandshakeID, hs.Encode())); err != nil {
		return "", err
	}

	var (
		peerID   int // the peer's ID for ut_metadata
		size     int
		data     []byte
		got      []bool
		received int
		next     int // next piece to request
	)

	numPieces := func() int { return (size + opts.PieceSize - 1) / opts.PieceSize }

	request := func() error {
		for ; next < numPieces() && next-received < opts.Window; next++ {
			m := MetadataMsg{Type: MetadataRequest, Piece: next}
			if err := c.Send(MsgExtended(byte(peerID), m.Encode())); err != nil {
				return err
			}
		}
		return nil
	}

	for {
		var msg Message
		var ok bool
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case msg, ok = <-c.Messages():
			if !ok {
				return "", c.Err()
			}
		}

		if msg.ID != Extended {
			continue
		}
		extID, payload, err := msg.ParseExtended()
		if err != nil {
			return "", err
		}

		switch {
		case extID == ExtHandshakeID && peerID == 0:
			h, err := ParseExtHandshake(payload)
			if err != nil {
				return "", err
			}
			peerID = h.M[UTMetadata]
			if peerID == 0 {
				return "", ErrNoUTMetadata
			}
			if h.MetadataSize <= 0 || h.MetadataSize > opts.MaxSize {
				return "", fmt.Errorf("peer reported metadata size %d", h.MetadataSize)
			}
			size = h.MetadataSize
			data = make([]byte, size)
			got = make([]bool, numPieces())
			if err := request(); err != nil {
				return "", err
			}

		case extID == UTMetadataID && peerID != 0:
			m, err := ParseMetadataMsg(payload)
			if err != nil {
				return "", err
			}
			switch m.Type {
			case MetadataReject:
				return "", fmt.Errorf("%w: piece %d", ErrMetadataRejected, m.Piece)
			case MetadataData:
			default:
				continue
			}

			if m.Piece < 0 || m.Piece >= len(got) || m.TotalSize != size {
				return "", fmt.Errorf("unexpected metadata piece %d (total size %d)", m.Piece, m.TotalSize)
			}
			begin := m.Piece * opts.PieceSize
			if want := min(opts.PieceSize, size-begin); len(m.Data) != want {
				return "", fmt.Errorf("metadata piece %d is %d bytes, want %d", m.Piece, len(m.Data), want)
			}
			if got[m.Piece] {
				continue
			}
			copy(data[begin:], m.Data)
			got[m.Piece] = true
			received++

			if received == len(got) {
				if sha1.Sum(data) != infoHash {
					return "", ErrMetadataHash
				}
				return string(data), nil
			}
			if err := request(); err != nil {
				return "", err
			}
		}
	}
}
