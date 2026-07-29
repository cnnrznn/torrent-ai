package metainfo

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/cnnrznn/torrent-scratch/bencoding"
)

// Metainfo represents the contents of a .torrent file
// https://www.bittorrent.org/beps/bep_0003.html
type Metainfo struct {
	announce string
	info     Info
}

type Info struct {
	name        string
	pieceLength int
	pieces      []string
	length      int
	files       []File
}

type File struct {
	length int
	path   []string
}

func Parse(s string) Metainfo {
	root := bencoding.ParseDictionary(s)
	rawInfoDict := bencoding.ParseDictionary(root["info"])
	files := []File{}

	if lengthStr, ok := rawInfoDict["length"]; ok {
		files = append(files, File{
			length: bencoding.ParseInteger(lengthStr),
		})
	}

	if fileListStr, ok := rawInfoDict["files"]; ok {
		fileList := bencoding.ParseList(fileListStr)
		for _, fileStr := range fileList {
			rawFileDict := bencoding.ParseDictionary(fileStr)
			files = append(files, File{
				length: bencoding.ParseInteger(rawFileDict["length"]),
				path:   bencoding.ParseList(rawFileDict["path"]),
			})
		}
	}

	return Metainfo{
		announce: bencoding.ParseString(root["announce"]),
		info: Info{
			name:        bencoding.ParseString(rawInfoDict["name"]),
			pieceLength: bencoding.ParseInteger(rawInfoDict["piece length"]),
			pieces:      splitPieceHashes(bencoding.ParseString(rawInfoDict["pieces"])),
		},
	}
}

func splitPieceHashes(s string) []string {
	hashes := []string{}

	for start := 0; start < len(s); start += 20 {
		hashes = append(hashes, s[start:start+20])
	}

	return hashes
}
