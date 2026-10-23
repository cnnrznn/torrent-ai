package peerwire

import (
	"encoding/binary"
	"fmt"
	"io"
)

// MessageID identifies the type of a peer wire message.
type MessageID byte

const (
	Choke         MessageID = 0
	Unchoke       MessageID = 1
	Interested    MessageID = 2
	NotInterested MessageID = 3
	Have          MessageID = 4
	BitfieldMsg   MessageID = 5
	Request       MessageID = 6
	Piece         MessageID = 7
	Cancel        MessageID = 8
	Port          MessageID = 9
	Extended      MessageID = 20
)

// MaxMessageLen bounds the size of a message we'll read. It fits a
// 16 KiB block with room to spare, and a bitfield for ~16M pieces.
const MaxMessageLen = 2 << 20

// BlockSize is the size of the blocks we request.
const BlockSize = 16 * 1024

// Message is a peer wire message, minus its length prefix.
type Message struct {
	ID      MessageID
	Payload []byte
}

func (id MessageID) String() string {
	switch id {
	case Choke:
		return "choke"
	case Unchoke:
		return "unchoke"
	case Interested:
		return "interested"
	case NotInterested:
		return "not interested"
	case Have:
		return "have"
	case BitfieldMsg:
		return "bitfield"
	case Request:
		return "request"
	case Piece:
		return "piece"
	case Cancel:
		return "cancel"
	case Port:
		return "port"
	case Extended:
		return "extended"
	}
	return fmt.Sprintf("unknown(%d)", byte(id))
}

// ReadMessage reads one message. It returns nil for a keep-alive.
func ReadMessage(r io.Reader) (*Message, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, err
	}

	n := binary.BigEndian.Uint32(lenBuf[:])
	if n == 0 {
		return nil, nil
	}
	if n > MaxMessageLen {
		return nil, fmt.Errorf("message length %d exceeds limit", n)
	}

	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}

	return &Message{ID: MessageID(b[0]), Payload: b[1:]}, nil
}

// Bytes encodes the message with its length prefix.
func (m Message) Bytes() []byte {
	b := make([]byte, 4+1+len(m.Payload))
	binary.BigEndian.PutUint32(b, uint32(1+len(m.Payload)))
	b[4] = byte(m.ID)
	copy(b[5:], m.Payload)
	return b
}

// KeepAlive is the encoded keep-alive message.
var KeepAlive = []byte{0, 0, 0, 0}

func MsgHave(index int) Message {
	return Message{ID: Have, Payload: binary.BigEndian.AppendUint32(nil, uint32(index))}
}

func MsgBitfield(bf Bitfield) Message {
	return Message{ID: BitfieldMsg, Payload: append([]byte(nil), bf...)}
}

func MsgRequest(index, begin, length int) Message {
	return Message{ID: Request, Payload: encodeInts(index, begin, length)}
}

func MsgCancel(index, begin, length int) Message {
	return Message{ID: Cancel, Payload: encodeInts(index, begin, length)}
}

func MsgPiece(index, begin int, block []byte) Message {
	return Message{ID: Piece, Payload: append(encodeInts(index, begin), block...)}
}

func MsgPort(port uint16) Message {
	return Message{ID: Port, Payload: binary.BigEndian.AppendUint16(nil, port)}
}

func MsgExtended(extID byte, payload []byte) Message {
	return Message{ID: Extended, Payload: append([]byte{extID}, payload...)}
}

// ParseHave returns the piece index of a have message.
func (m Message) ParseHave() (int, error) {
	if len(m.Payload) != 4 {
		return 0, fmt.Errorf("have payload is %d bytes", len(m.Payload))
	}
	return int(binary.BigEndian.Uint32(m.Payload)), nil
}

// ParseRequest parses a request or cancel message.
func (m Message) ParseRequest() (index, begin, length int, err error) {
	if len(m.Payload) != 12 {
		return 0, 0, 0, fmt.Errorf("%s payload is %d bytes", m.ID, len(m.Payload))
	}
	return int(binary.BigEndian.Uint32(m.Payload)),
		int(binary.BigEndian.Uint32(m.Payload[4:])),
		int(binary.BigEndian.Uint32(m.Payload[8:])),
		nil
}

// ParsePiece parses a piece message.
func (m Message) ParsePiece() (index, begin int, block []byte, err error) {
	if len(m.Payload) < 8 {
		return 0, 0, nil, fmt.Errorf("piece payload is %d bytes", len(m.Payload))
	}
	return int(binary.BigEndian.Uint32(m.Payload)),
		int(binary.BigEndian.Uint32(m.Payload[4:])),
		m.Payload[8:],
		nil
}

// ParsePort parses a port message.
func (m Message) ParsePort() (uint16, error) {
	if len(m.Payload) != 2 {
		return 0, fmt.Errorf("port payload is %d bytes", len(m.Payload))
	}
	return binary.BigEndian.Uint16(m.Payload), nil
}

// ParseExtended splits an extended message into its extension ID and
// payload.
func (m Message) ParseExtended() (byte, []byte, error) {
	if len(m.Payload) < 1 {
		return 0, nil, fmt.Errorf("extended message is empty")
	}
	return m.Payload[0], m.Payload[1:], nil
}

func encodeInts(xs ...int) []byte {
	b := make([]byte, 0, 4*len(xs))
	for _, x := range xs {
		b = binary.BigEndian.AppendUint32(b, uint32(x))
	}
	return b
}
