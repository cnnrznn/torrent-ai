package metainfo

import (
	"crypto/sha1"
	"errors"
	"fmt"
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
	rawInfo  string
	infoHash id.Hash
}

func (m *Metainfo) Announce() string {
	return m.announce
}

func (m *Metainfo) InfoHash() id.Hash {
	return m.infoHash
}

// RawInfo returns the bencoded info dictionary.
func (m *Metainfo) RawInfo() string {
	return m.rawInfo
}

func (m *Metainfo) Name() string {
	return m.info.name
}

func (m *Metainfo) PieceLength() int {
	return m.info.pieceLength
}

func (m *Metainfo) NumPieces() int {
	return len(m.info.pieces)
}

// PieceHash returns the SHA-1 hash of piece i.
func (m *Metainfo) PieceHash(i int) id.Hash {
	return m.info.pieces[i]
}

// PieceSize returns the size of piece i. Only the last piece can be
// shorter than PieceLength.
func (m *Metainfo) PieceSize(i int) int {
	if i == len(m.info.pieces)-1 {
		return m.Length() - i*m.info.pieceLength
	}
	return m.info.pieceLength
}

// Length returns the total size of the torrent's content in bytes.
func (m *Metainfo) Length() int {
	length := m.info.length
	for _, f := range m.info.files {
		length += f.Length
	}
	return length
}

// Files returns the files in the torrent, in order. Each path starts
// with the torrent's name, so a single-file torrent has one file whose
// path is just the name.
func (m *Metainfo) Files() []File {
	if len(m.info.files) == 0 {
		return []File{{Length: m.info.length, Path: []string{m.info.name}}}
	}

	files := make([]File, len(m.info.files))
	for i, f := range m.info.files {
		files[i] = File{
			Length: f.Length,
			Path:   append([]string{m.info.name}, f.Path...),
		}
	}
	return files
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
	b.WriteString(strconv.Itoa(m.Length()))
	b.WriteByte('\n')

	for _, file := range m.info.files {
		b.WriteString("File: \n")
		b.WriteString("\tPath: ")
		b.WriteString(strings.Join(file.Path, "/"))
		b.WriteByte('\n')
		b.WriteString("\tLength: ")
		b.WriteString(strconv.Itoa(file.Length))
		b.WriteByte('\n')
	}

	return b.String()
}

type Info struct {
	name        string
	pieceLength int
	pieces      []id.Hash
	length      int
	files       []File
}

type File struct {
	Length int
	Path   []string
}

// New parses the contents of a .torrent file.
func New(s string) (*Metainfo, error) {
	root, _, err := bencoding.ParseDictionary(s)
	if err != nil {
		return nil, fmt.Errorf("invalid torrent file: %w", err)
	}

	rawInfo, ok := root["info"]
	if !ok {
		return nil, errors.New("torrent file has no info dictionary")
	}

	m, err := FromInfoDict(rawInfo)
	if err != nil {
		return nil, err
	}

	if raw, ok := root["announce"]; ok {
		m.announce, _, err = bencoding.ParseString(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid announce: %w", err)
		}
	}

	return m, nil
}

// FromInfoDict builds a Metainfo from a bencoded info dictionary, such
// as one fetched from peers for a magnet link. The result has no
// announce URL.
func FromInfoDict(raw string) (*Metainfo, error) {
	d, size, err := bencoding.ParseDictionary(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid info dictionary: %w", err)
	}
	if size != len(raw) {
		return nil, errors.New("trailing data after info dictionary")
	}

	info, err := parseInfo(d)
	if err != nil {
		return nil, err
	}

	return &Metainfo{
		info:     info,
		rawInfo:  raw,
		infoHash: sha1.Sum([]byte(raw)),
	}, nil
}

func parseInfo(d map[string]string) (Info, error) {
	var info Info
	var err error

	info.name, _, err = bencoding.ParseString(d["name"])
	if err != nil {
		return info, fmt.Errorf("invalid name: %w", err)
	}
	if err := checkPathComponent(info.name); err != nil {
		return info, fmt.Errorf("invalid name: %w", err)
	}

	info.pieceLength, _, err = bencoding.ParseInteger(d["piece length"])
	if err != nil {
		return info, fmt.Errorf("invalid piece length: %w", err)
	}
	if info.pieceLength <= 0 {
		return info, fmt.Errorf("invalid piece length %d", info.pieceLength)
	}

	piecesStr, _, err := bencoding.ParseString(d["pieces"])
	if err != nil {
		return info, fmt.Errorf("invalid pieces: %w", err)
	}
	info.pieces, err = splitPieceHashes(piecesStr)
	if err != nil {
		return info, err
	}

	lengthStr, isSingle := d["length"]
	fileListStr, isMulti := d["files"]

	switch {
	case isSingle && isMulti:
		return info, errors.New("info has both length and files")
	case isSingle:
		info.length, _, err = bencoding.ParseInteger(lengthStr)
		if err != nil {
			return info, fmt.Errorf("invalid length: %w", err)
		}
		if info.length < 0 {
			return info, fmt.Errorf("invalid length %d", info.length)
		}
	case isMulti:
		info.files, err = parseFiles(fileListStr)
		if err != nil {
			return info, err
		}
	default:
		return info, errors.New("info has neither length nor files")
	}

	m := Metainfo{info: info}
	total := m.Length()
	want := (total + info.pieceLength - 1) / info.pieceLength
	if len(info.pieces) != want {
		return info, fmt.Errorf("%d bytes needs %d pieces, info has %d", total, want, len(info.pieces))
	}

	return info, nil
}

func parseFiles(s string) ([]File, error) {
	fileList, _, err := bencoding.ParseList(s)
	if err != nil {
		return nil, fmt.Errorf("invalid files: %w", err)
	}
	if len(fileList) == 0 {
		return nil, errors.New("files is empty")
	}

	var files []File

	for _, fileStr := range fileList {
		rawFileDict, _, err := bencoding.ParseDictionary(fileStr)
		if err != nil {
			return nil, fmt.Errorf("invalid file: %w", err)
		}
		fileSize, _, err := bencoding.ParseInteger(rawFileDict["length"])
		if err != nil {
			return nil, fmt.Errorf("invalid file length: %w", err)
		}
		if fileSize < 0 {
			return nil, fmt.Errorf("invalid file length %d", fileSize)
		}
		pathList, _, err := bencoding.ParseList(rawFileDict["path"])
		if err != nil {
			return nil, fmt.Errorf("invalid file path: %w", err)
		}
		path, err := parseStringList(pathList)
		if err != nil {
			return nil, fmt.Errorf("invalid file path: %w", err)
		}
		if len(path) == 0 {
			return nil, errors.New("file path is empty")
		}
		for _, c := range path {
			if err := checkPathComponent(c); err != nil {
				return nil, fmt.Errorf("invalid file path: %w", err)
			}
		}
		files = append(files, File{
			Length: fileSize,
			Path:   path,
		})
	}

	return files, nil
}

// checkPathComponent rejects names that could escape the download
// directory.
func checkPathComponent(s string) error {
	switch {
	case s == "", s == ".", s == "..":
		return fmt.Errorf("unsafe path component %q", s)
	case strings.ContainsAny(s, "/\\\x00"):
		return fmt.Errorf("unsafe path component %q", s)
	}
	return nil
}

func splitPieceHashes(s string) ([]id.Hash, error) {
	if len(s)%20 != 0 {
		return nil, fmt.Errorf("pieces length %d is not a multiple of 20", len(s))
	}

	hashes := []id.Hash{}

	for start := 0; start < len(s); start += 20 {
		h, _ := id.FromString(s[start : start+20])
		hashes = append(hashes, h)
	}

	return hashes, nil
}

// parseStringList parses a list of strings where each
// element is a bencoded string. It returns a list of
// decoded strings.
func parseStringList(rawList []string) ([]string, error) {
	var result []string

	for _, raw := range rawList {
		s, _, err := bencoding.ParseString(raw)
		if err != nil {
			return nil, err
		}
		result = append(result, s)
	}

	return result, nil
}
