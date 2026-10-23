package peerwire

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/cnnrznn/torrent-scratch/pkg/id"
)

func TestHandshake(t *testing.T) {
	t.Run("round trips", func(t *testing.T) {
		h := NewHandshake(id.Random(), id.NewPeerID(), true)
		b := h.Bytes()
		require.Len(t, b, HandshakeLen)

		got, err := ReadHandshake(bytes.NewReader(b))
		require.NoError(t, err)
		require.Equal(t, h, got)
		require.True(t, got.SupportsExtensions())
		require.True(t, got.SupportsDHT())
	})

	t.Run("dht bit is optional", func(t *testing.T) {
		h := NewHandshake(id.Random(), id.NewPeerID(), false)
		require.True(t, h.SupportsExtensions())
		require.False(t, h.SupportsDHT())
	})

	t.Run("rejects another protocol", func(t *testing.T) {
		b := NewHandshake(id.Random(), id.Random(), false).Bytes()
		b[1] = 'X'
		_, err := ReadHandshake(bytes.NewReader(b))
		require.Error(t, err)
	})

	t.Run("rejects truncated input", func(t *testing.T) {
		b := NewHandshake(id.Random(), id.Random(), false).Bytes()
		_, err := ReadHandshake(bytes.NewReader(b[:30]))
		require.Error(t, err)
	})
}

func TestMessages(t *testing.T) {
	t.Run("round trips through the wire format", func(t *testing.T) {
		var buf bytes.Buffer
		buf.Write(MsgRequest(1, 2, 3).Bytes())
		buf.Write(KeepAlive)
		buf.Write(Message{ID: Unchoke}.Bytes())

		m, err := ReadMessage(&buf)
		require.NoError(t, err)
		index, begin, length, err := m.ParseRequest()
		require.NoError(t, err)
		require.Equal(t, []int{1, 2, 3}, []int{index, begin, length})

		m, err = ReadMessage(&buf)
		require.NoError(t, err)
		require.Nil(t, m)

		m, err = ReadMessage(&buf)
		require.NoError(t, err)
		require.Equal(t, Unchoke, m.ID)
		require.Empty(t, m.Payload)
	})

	t.Run("parses each payload", func(t *testing.T) {
		i, err := MsgHave(7).ParseHave()
		require.NoError(t, err)
		require.Equal(t, 7, i)

		index, begin, block, err := MsgPiece(4, 16384, []byte("data")).ParsePiece()
		require.NoError(t, err)
		require.Equal(t, 4, index)
		require.Equal(t, 16384, begin)
		require.Equal(t, []byte("data"), block)

		port, err := MsgPort(6881).ParsePort()
		require.NoError(t, err)
		require.EqualValues(t, 6881, port)

		ext, payload, err := MsgExtended(3, []byte("x")).ParseExtended()
		require.NoError(t, err)
		require.EqualValues(t, 3, ext)
		require.Equal(t, []byte("x"), payload)
	})

	t.Run("rejects short payloads", func(t *testing.T) {
		_, err := Message{ID: Have}.ParseHave()
		require.Error(t, err)
		_, _, _, err = Message{ID: Request, Payload: []byte{1}}.ParseRequest()
		require.Error(t, err)
		_, _, _, err = Message{ID: Piece}.ParsePiece()
		require.Error(t, err)
	})

	t.Run("rejects oversized messages", func(t *testing.T) {
		_, err := ReadMessage(bytes.NewReader([]byte{0xff, 0xff, 0xff, 0xff}))
		require.ErrorContains(t, err, "exceeds limit")
	})
}

func TestBitfield(t *testing.T) {
	bf := NewBitfield(10)
	require.Len(t, bf, 2)

	bf.Set(0)
	bf.Set(9)
	bf.Set(16) // past the end, ignored
	require.True(t, bf.Has(0))
	require.True(t, bf.Has(9))
	require.False(t, bf.Has(1))
	require.False(t, bf.Has(16))
	require.False(t, bf.Has(-1))
	require.Equal(t, 2, bf.Count())
	require.Equal(t, Bitfield{0x80, 0x40}, bf)

	_, err := ParseBitfield([]byte{0x80, 0x40}, 10)
	require.NoError(t, err)
	_, err = ParseBitfield([]byte{0x80}, 10)
	require.Error(t, err)
	_, err = ParseBitfield([]byte{0x80, 0x20}, 10)
	require.ErrorContains(t, err, "spare bit")
}

func TestClientName(t *testing.T) {
	h, _ := id.FromString("-qB4630-abcdefghijkl")
	require.Equal(t, "qB4630", ClientName(h))
	h, _ = id.FromString("M7-4-0--\x00\x01cdefghijkl")
	require.Equal(t, "M7-4-0--", ClientName(h))
	h, _ = id.FromString("\x00\x01\x02\x03\x04\x05\x06\x07abcdefghijkl")
	require.Equal(t, "........", ClientName(h))
}
