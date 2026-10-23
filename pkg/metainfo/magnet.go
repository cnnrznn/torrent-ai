package metainfo

import (
	"encoding/base32"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/cnnrznn/torrent-ai/pkg/id"
)

// Magnet is a parsed magnet link (BEP 9).
type Magnet struct {
	InfoHash id.Hash
	// Name is the display name (dn), if any.
	Name string
	// Trackers are the announce URLs (tr), if any.
	Trackers []string
}

const btihPrefix = "urn:btih:"

// ParseMagnet parses a magnet URI. The infohash may be 40 hex
// characters or 32 base32 characters.
func ParseMagnet(uri string) (*Magnet, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "magnet" {
		return nil, fmt.Errorf("not a magnet link: scheme %q", u.Scheme)
	}

	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, err
	}

	m := &Magnet{
		Name:     q.Get("dn"),
		Trackers: q["tr"],
	}

	found := false
	for _, xt := range q["xt"] {
		if !strings.HasPrefix(strings.ToLower(xt), btihPrefix) {
			continue
		}
		m.InfoHash, err = parseBTIH(xt[len(btihPrefix):])
		if err != nil {
			return nil, err
		}
		found = true
		break
	}
	if !found {
		return nil, errors.New("magnet link has no urn:btih infohash")
	}

	return m, nil
}

func parseBTIH(s string) (id.Hash, error) {
	switch len(s) {
	case 40:
		return id.FromHex(s)
	case 32:
		b, err := base32.StdEncoding.DecodeString(strings.ToUpper(s))
		if err != nil {
			return id.Hash{}, fmt.Errorf("invalid base32 infohash: %w", err)
		}
		return id.FromString(string(b))
	}
	return id.Hash{}, fmt.Errorf("infohash %q is neither 40 hex nor 32 base32 characters", s)
}
