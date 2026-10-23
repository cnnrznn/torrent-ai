package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cnnrznn/torrent-ai/pkg/dht"
	"github.com/cnnrznn/torrent-ai/pkg/id"
)

// Record is what the Store keeps for each torrent, enough to add it
// again after a restart.
type Record struct {
	InfoHash string    `json:"infohash"`
	Magnet   string    `json:"magnet,omitempty"`
	Torrent  []byte    `json:"torrent,omitempty"` // a .torrent file
	Dir      string    `json:"dir"`
	Peers    []string  `json:"peers,omitempty"`
	Trackers []string  `json:"trackers,omitempty"`
	AddedAt  time.Time `json:"added_at"`
}

// Store keeps the daemon's state in a directory:
//
//	torrents/<infohash>.json  one Record per torrent
//	info/<infohash>.info      the bencoded info dict, once known
//	dht-nodes.json            saved DHT routing table (see SaveDHTNodes)
//
// Writes go to a temporary file renamed into place, so a crash never
// leaves a half-written file.
type Store struct {
	dir string
}

// OpenStore opens or creates a state directory.
func OpenStore(dir string) (*Store, error) {
	for _, sub := range []string{"torrents", "info"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, err
		}
	}
	return &Store{dir: dir}, nil
}

// Dir returns the state directory.
func (s *Store) Dir() string {
	return s.dir
}

func (s *Store) recordPath(h id.Hash) string {
	return filepath.Join(s.dir, "torrents", h.Hex()+".json")
}

func (s *Store) infoPath(h id.Hash) string {
	return filepath.Join(s.dir, "info", h.Hex()+".info")
}

// Save writes a torrent's record.
func (s *Store) Save(r Record) error {
	h, err := id.FromHex(r.InfoHash)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.recordPath(h), b)
}

// SaveInfo caches a torrent's info dict, so a magnet link needn't fetch
// it again after a restart.
func (s *Store) SaveInfo(h id.Hash, raw string) error {
	return writeFileAtomic(s.infoPath(h), []byte(raw))
}

// LoadInfo returns a cached info dict, if there is one.
func (s *Store) LoadInfo(h id.Hash) (string, bool, error) {
	b, err := os.ReadFile(s.infoPath(h))
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return string(b), true, nil
}

// Delete forgets a torrent.
func (s *Store) Delete(h id.Hash) error {
	var errs []error
	for _, p := range []string{s.recordPath(h), s.infoPath(h)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Records returns every saved record.
func (s *Store) Records() ([]Record, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "torrents"))
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.dir, "torrents", e.Name()))
		if err != nil {
			return nil, err
		}
		var r Record
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		out = append(out, r)
	}
	return out, nil
}

// DHTState is the DHT node's identity and routing table, saved so a
// restart keeps its node ID and doesn't bootstrap from scratch.
type DHTState struct {
	ID      string         `json:"id"`
	Nodes   []dht.NodeInfo `json:"nodes"`
	SavedAt time.Time      `json:"saved_at"`
}

func (s *Store) dhtPath() string {
	return filepath.Join(s.dir, "dht-nodes.json")
}

// SaveDHT saves the DHT node's state.
func (s *Store) SaveDHT(st DHTState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.dhtPath(), b)
}

// LoadDHT returns the saved DHT state, if any.
func (s *Store) LoadDHT() (DHTState, bool, error) {
	var st DHTState
	b, err := os.ReadFile(s.dhtPath())
	if errors.Is(err, fs.ErrNotExist) {
		return st, false, nil
	}
	if err != nil {
		return st, false, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, false, fmt.Errorf("dht-nodes.json: %w", err)
	}
	return st, true, nil
}

func writeFileAtomic(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(b)
	err = errors.Join(err, tmp.Sync(), tmp.Close())
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}
