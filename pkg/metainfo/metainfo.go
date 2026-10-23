package metainfo

import (
	"crypto/sha1"
	"strconv"
	"strings"

	"github.com/cnnrznn/torrent-scratch/pkg/bencoding"
	"github.com/cnnrznn/torrent-scratch/pkg/id"
)

// Metainfo represents the contents of a .torrent file
// https://www.bittorrent.org/beps/bep_0003.html
type Metainfo struct {
	announce string
	info     Info

	// rawInfo is the bencoded value of the info dictionary
	rawInfo string
}

func (m *Metainfo) Announce() string {
	return m.announce
}

func (m *Metainfo) InfoHash() id.Hash {
	return sha1.Sum([]byte(m.rawInfo))
}

func (m *Metainfo) String() string {
	var b strings.Builder

	b.WriteString("Announce: ")
	b.WriteString(m.announce)
	b.WriteByte('\n')
	b.WriteString("Name: ")
	b.WriteString(m.info.name)
	b.WriteByte('\n')
	b.WriteString("Piece Length: ")
	b.WriteString(strconv.Itoa(m.info.pieceLength))
	b.WriteByte('\n')
	b.WriteString("Length: ")
	b.WriteString(strconv.Itoa(m.info.length))
	b.WriteByte('\n')

	for _, file := range m.info.files {
		b.WriteString("File: \n")
		b.WriteString("\tPath: ")
		b.WriteString(strings.Join(file.path, "/"))
		b.WriteByte('\n')
		b.WriteString("\tLength: ")
		b.WriteString(strconv.Itoa(file.length))
		b.WriteByte('\n')
	}

	return b.String()
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

func New(s string) *Metainfo {
	root, _, _ := bencoding.ParseDictionary(s)
	rawInfoDict, _, _ := bencoding.ParseDictionary(root["info"])
	var files []File
	var length int

	if lengthStr, ok := rawInfoDict["length"]; ok {
		length, _, _ = bencoding.ParseInteger(lengthStr)
	}

	if fileListStr, ok := rawInfoDict["files"]; ok {
		fileList, _, _ := bencoding.ParseList(fileListStr)
		for _, fileStr := range fileList {
			var (
				rawFileDict, _, _ = bencoding.ParseDictionary(fileStr)
				fileSize, _, _    = bencoding.ParseInteger(rawFileDict["length"])
				pathList, _, _    = bencoding.ParseList(rawFileDict["path"])
			)
			files = append(files, File{
				length: fileSize,
				path:   parseStringList(pathList),
			})
		}
	}

	var (
		annonceURL, _, _  = bencoding.ParseString(root["announce"])
		name, _, _        = bencoding.ParseString(rawInfoDict["name"])
		pieceLength, _, _ = bencoding.ParseInteger(rawInfoDict["piece length"])
		piecesStr, _, _   = bencoding.ParseString(rawInfoDict["pieces"])
	)

	return &Metainfo{
		announce: annonceURL,
		info: Info{
			name:        name,
			pieceLength: pieceLength,
			pieces:      splitPieceHashes(piecesStr),
			length:      length,
			files:       files,
		},
		rawInfo: root["info"],
	}
}

func splitPieceHashes(s string) []string {
	hashes := []string{}

	for start := 0; start < len(s); start += 20 {
		hashes = append(hashes, s[start:start+20])
	}

	return hashes
}

// parseStringList parses a list of strings where each
// element is a bencoded string. It returns a list of
// decoded strings.
func parseStringList(rawList []string) []string {
	var result []string

	for _, raw := range rawList {
		s, _, _ := bencoding.ParseString(raw)
		result = append(result, s)
	}

	return result
}
