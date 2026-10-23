// Package torrent runs a download session for a single torrent.
package torrent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/cnnrznn/torrent-scratch/pkg/metainfo"
)

type storageFile struct {
	f      *os.File
	offset int64
	length int64
}

// Storage maps the torrent's byte stream onto its files.
type Storage struct {
	files  []storageFile
	length int64
}

// OpenStorage opens or creates the torrent's files under dir. existed
// reports whether any of them were already there, in which case their
// contents should be verified.
func OpenStorage(dir string, m *metainfo.Metainfo) (s *Storage, existed bool, err error) {
	s = &Storage{}
	defer func() {
		if err != nil {
			s.Close()
		}
	}()

	var offset int64
	for _, file := range m.Files() {
		path := filepath.Join(append([]string{dir}, file.Path...)...)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, false, err
		}

		if _, err := os.Stat(path); err == nil {
			existed = true
		}

		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			return nil, false, err
		}
		if err := f.Truncate(int64(file.Length)); err != nil {
			f.Close()
			return nil, false, err
		}

		s.files = append(s.files, storageFile{f: f, offset: offset, length: int64(file.Length)})
		offset += int64(file.Length)
	}
	s.length = offset

	return s, existed, nil
}

// WriteAt writes p at offset off in the torrent's byte stream.
func (s *Storage) WriteAt(p []byte, off int64) error {
	return s.span(p, off, func(f *os.File, b []byte, at int64) error {
		_, err := f.WriteAt(b, at)
		return err
	})
}

// ReadAt fills p from offset off in the torrent's byte stream.
func (s *Storage) ReadAt(p []byte, off int64) error {
	return s.span(p, off, func(f *os.File, b []byte, at int64) error {
		_, err := f.ReadAt(b, at)
		if errors.Is(err, io.EOF) {
			err = nil
		}
		return err
	})
}

// span calls fn for each file that p covers when placed at off.
func (s *Storage) span(p []byte, off int64, fn func(*os.File, []byte, int64) error) error {
	if off < 0 || off+int64(len(p)) > s.length {
		return fmt.Errorf("range [%d, %d) is outside the torrent", off, off+int64(len(p)))
	}

	for _, sf := range s.files {
		if len(p) == 0 {
			break
		}
		if off >= sf.offset+sf.length {
			continue
		}

		at := off - sf.offset
		n := min(int64(len(p)), sf.length-at)
		if err := fn(sf.f, p[:n], at); err != nil {
			return err
		}
		p = p[n:]
		off += n
	}

	return nil
}

// Close closes all files.
func (s *Storage) Close() error {
	var errs []error
	for _, sf := range s.files {
		errs = append(errs, sf.f.Close())
	}
	return errors.Join(errs...)
}
