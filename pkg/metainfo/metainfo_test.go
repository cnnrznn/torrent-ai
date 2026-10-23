package metainfo

import (
	"crypto/sha1"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/cnnrznn/torrent-scratch/pkg/bencoding"
)

func singleFileInfo(length, pieceLength int) string {
	pieces := (length + pieceLength - 1) / pieceLength
	return bencoding.EncodeDictionary(map[string]string{
		"name":         bencoding.EncodeString("file.bin"),
		"length":       bencoding.EncodeInteger(length),
		"piece length": bencoding.EncodeInteger(pieceLength),
		"pieces":       bencoding.EncodeString(strings.Repeat("x", 20*pieces)),
	})
}

func multiFileInfo(paths ...[]string) string {
	var files []string
	for _, p := range paths {
		var parts []string
		for _, c := range p {
			parts = append(parts, bencoding.EncodeString(c))
		}
		files = append(files, bencoding.EncodeDictionary(map[string]string{
			"length": bencoding.EncodeInteger(10),
			"path":   bencoding.EncodeList(parts...),
		}))
	}
	pieces := (10*len(paths) + 15) / 16
	return bencoding.EncodeDictionary(map[string]string{
		"name":         bencoding.EncodeString("dir"),
		"files":        bencoding.EncodeList(files...),
		"piece length": bencoding.EncodeInteger(16),
		"pieces":       bencoding.EncodeString(strings.Repeat("x", 20*pieces)),
	})
}

func TestNew(t *testing.T) {
	t.Run("parses a single-file torrent", func(t *testing.T) {
		info := singleFileInfo(100, 32)
		m, err := New(bencoding.EncodeDictionary(map[string]string{
			"announce": bencoding.EncodeString("http://tracker/announce"),
			"info":     info,
		}))
		require.NoError(t, err)

		require.Equal(t, "http://tracker/announce", m.Announce())
		require.Equal(t, sha1.Sum([]byte(info)), [20]byte(m.InfoHash()))
		require.Equal(t, "file.bin", m.Name())
		require.Equal(t, 100, m.Length())
		require.Equal(t, 4, m.NumPieces())
		require.Equal(t, 32, m.PieceSize(0))
		require.Equal(t, 4, m.PieceSize(3))
		require.Equal(t, []File{{Length: 100, Path: []string{"file.bin"}}}, m.Files())
	})

	t.Run("parses a multi-file torrent", func(t *testing.T) {
		m, err := FromInfoDict(multiFileInfo([]string{"a"}, []string{"sub", "b"}))
		require.NoError(t, err)

		require.Equal(t, 20, m.Length())
		require.Equal(t, []File{
			{Length: 10, Path: []string{"dir", "a"}},
			{Length: 10, Path: []string{"dir", "sub", "b"}},
		}, m.Files())
	})

	t.Run("rejects garbage", func(t *testing.T) {
		_, err := New("not bencoding")
		require.Error(t, err)
		_, err = New("de")
		require.ErrorContains(t, err, "no info dictionary")
	})
}

func TestFromInfoDictValidation(t *testing.T) {
	t.Run("rejects the wrong number of pieces", func(t *testing.T) {
		info := bencoding.EncodeDictionary(map[string]string{
			"name":         bencoding.EncodeString("f"),
			"length":       bencoding.EncodeInteger(100),
			"piece length": bencoding.EncodeInteger(32),
			"pieces":       bencoding.EncodeString(strings.Repeat("x", 20)),
		})
		_, err := FromInfoDict(info)
		require.ErrorContains(t, err, "needs 4 pieces")
	})

	t.Run("rejects a zero piece length", func(t *testing.T) {
		info := bencoding.EncodeDictionary(map[string]string{
			"name":         bencoding.EncodeString("f"),
			"length":       bencoding.EncodeInteger(0),
			"piece length": bencoding.EncodeInteger(0),
			"pieces":       bencoding.EncodeString(""),
		})
		_, err := FromInfoDict(info)
		require.ErrorContains(t, err, "invalid piece length 0")
	})

	t.Run("rejects trailing data", func(t *testing.T) {
		_, err := FromInfoDict(singleFileInfo(10, 16) + "x")
		require.ErrorContains(t, err, "trailing data")
	})

	for _, path := range [][]string{{".."}, {"a", "..", "b"}, {"/etc"}, {""}, {}} {
		t.Run("rejects unsafe path "+strings.Join(path, "/"), func(t *testing.T) {
			_, err := FromInfoDict(multiFileInfo(path))
			require.Error(t, err)
		})
	}
}
