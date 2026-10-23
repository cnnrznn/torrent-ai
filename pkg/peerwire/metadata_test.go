package peerwire

import (
	"context"
	"crypto/sha1"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/cnnrznn/torrent-ai/pkg/id"
)

func TestExtHandshake(t *testing.T) {
	h := ExtHandshake{M: map[string]int{"ut_metadata": 3, "ut_pex": 1}, MetadataSize: 31235, V: "test 1.0"}
	got, err := ParseExtHandshake(h.Encode())
	require.NoError(t, err)
	require.Equal(t, h, got)

	got, err = ParseExtHandshake([]byte("d1:md11:ut_metadatai0e3:badi999eee"))
	require.NoError(t, err)
	require.Empty(t, got.M, "zero and out-of-range IDs are dropped")

	_, err = ParseExtHandshake([]byte("garbage"))
	require.Error(t, err)
}

func TestMetadataMsg(t *testing.T) {
	m := MetadataMsg{Type: MetadataData, Piece: 2, TotalSize: 40000, Data: []byte("raw bytes")}
	got, err := ParseMetadataMsg(m.Encode())
	require.NoError(t, err)
	require.Equal(t, m, got)

	m = MetadataMsg{Type: MetadataRequest, Piece: 0}
	got, err = ParseMetadataMsg(m.Encode())
	require.NoError(t, err)
	require.Equal(t, m, got)
}

// serveMetadata answers ut_metadata requests on c from info. mutate
// lets a test corrupt the replies.
func serveMetadata(c *Conn, info []byte, mutate func(*MetadataMsg)) {
	hs := ExtHandshake{M: map[string]int{UTMetadata: 7}, MetadataSize: len(info)}
	c.Send(MsgExtended(ExtHandshakeID, hs.Encode()))

	var theirID int
	for msg := range c.Messages() {
		extID, payload, err := msg.ParseExtended()
		if err != nil {
			continue
		}
		if extID == ExtHandshakeID {
			h, _ := ParseExtHandshake(payload)
			theirID = h.M[UTMetadata]
			continue
		}
		req, err := ParseMetadataMsg(payload)
		if err != nil || extID != 7 {
			continue
		}
		begin := req.Piece * MetadataPieceSize
		res := MetadataMsg{
			Type:      MetadataData,
			Piece:     req.Piece,
			TotalSize: len(info),
			Data:      info[begin:min(begin+MetadataPieceSize, len(info))],
		}
		if mutate != nil {
			mutate(&res)
		}
		c.Send(MsgExtended(byte(theirID), res.Encode()))
	}
}

func TestFetchMetadata(t *testing.T) {
	// More pieces than the request window, with a short last piece.
	info := make([]byte, 20*MetadataPieceSize+123)
	r := rand.New(rand.NewPCG(1, 2))
	for i := range info {
		info[i] = byte(r.UintN(256))
	}
	infoHash := id.Hash(sha1.Sum(info))

	fetch := func(t *testing.T, mutate func(*MetadataMsg)) (string, error) {
		client, server, err := connPair(t, infoHash, infoHash)
		require.NoError(t, err)
		defer client.Close()
		defer server.Close()
		go serveMetadata(server, info, mutate)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return FetchMetadata(ctx, client, infoHash)
	}

	t.Run("downloads and verifies the info dict", func(t *testing.T) {
		got, err := fetch(t, nil)
		require.NoError(t, err)
		require.Equal(t, string(info), got)
	})

	t.Run("rejects metadata with the wrong hash", func(t *testing.T) {
		_, err := fetch(t, func(m *MetadataMsg) {
			if m.Piece == 3 {
				m.Data = append([]byte(nil), m.Data...)
				m.Data[0] ^= 1
			}
		})
		require.ErrorIs(t, err, ErrMetadataHash)
	})

	t.Run("fails when the peer rejects a piece", func(t *testing.T) {
		_, err := fetch(t, func(m *MetadataMsg) {
			if m.Piece == 1 {
				*m = MetadataMsg{Type: MetadataReject, Piece: 1}
			}
		})
		require.ErrorIs(t, err, ErrMetadataRejected)
	})

	t.Run("rejects a short piece", func(t *testing.T) {
		_, err := fetch(t, func(m *MetadataMsg) { m.Data = m.Data[:10] })
		require.ErrorContains(t, err, "want")
	})
}
