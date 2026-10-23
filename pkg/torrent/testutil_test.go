package torrent

import (
	"crypto/sha1"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/cnnrznn/torrent-scratch/pkg/bencoding"
	"github.com/cnnrznn/torrent-scratch/pkg/metainfo"
)

// makeTorrent builds a multi-file torrent over random content and
// returns it with the content.
func makeTorrent(t *testing.T, pieceLength int, fileLengths ...int) (*metainfo.Metainfo, []byte) {
	t.Helper()

	r := rand.New(rand.NewPCG(1, 2))
	var data []byte
	var files []string
	for i, n := range fileLengths {
		b := make([]byte, n)
		for j := range b {
			b[j] = byte(r.UintN(256))
		}
		data = append(data, b...)
		files = append(files, bencoding.EncodeDictionary(map[string]string{
			"length": bencoding.EncodeInteger(n),
			"path":   bencoding.EncodeList(bencoding.EncodeString("f"), bencoding.EncodeString(string(rune('a'+i)))),
		}))
	}

	var pieces []byte
	for off := 0; off < len(data); off += pieceLength {
		h := sha1.Sum(data[off:min(off+pieceLength, len(data))])
		pieces = append(pieces, h[:]...)
	}

	info := bencoding.EncodeDictionary(map[string]string{
		"name":         bencoding.EncodeString("test"),
		"piece length": bencoding.EncodeInteger(pieceLength),
		"pieces":       bencoding.EncodeString(string(pieces)),
		"files":        bencoding.EncodeList(files...),
	})
	m, err := metainfo.FromInfoDict(info)
	require.NoError(t, err)

	return m, data
}
