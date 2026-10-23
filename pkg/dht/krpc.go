// Package dht implements the BitTorrent Mainline DHT (BEP 5).
package dht

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"

	"github.com/cnnrznn/torrent-scratch/pkg/bencoding"
	"github.com/cnnrznn/torrent-scratch/pkg/id"
)

// KRPC error codes.
const (
	errGeneric  = 201
	errProtocol = 203
	errMethod   = 204
)

// msg is a decoded KRPC message. Arguments and return values are kept
// as raw bencoded values, as the bencoding parser returns them.
type msg struct {
	T string // transaction ID
	Y string // "q", "r" or "e"

	Q string            // query method, for "q"
	A map[string]string // query arguments, for "q"

	R map[string]string // return values, for "r"

	ErrCode int    // for "e"
	ErrMsg  string // for "e"
}

func encodeQuery(t, q string, args map[string]string) []byte {
	return []byte(bencoding.EncodeDictionary(map[string]string{
		"t": bencoding.EncodeString(t),
		"y": bencoding.EncodeString("q"),
		"q": bencoding.EncodeString(q),
		"a": bencoding.EncodeDictionary(args),
	}))
}

func encodeResponse(t string, r map[string]string) []byte {
	return []byte(bencoding.EncodeDictionary(map[string]string{
		"t": bencoding.EncodeString(t),
		"y": bencoding.EncodeString("r"),
		"r": bencoding.EncodeDictionary(r),
	}))
}

func encodeError(t string, code int, message string) []byte {
	return []byte(bencoding.EncodeDictionary(map[string]string{
		"t": bencoding.EncodeString(t),
		"y": bencoding.EncodeString("e"),
		"e": bencoding.EncodeList(bencoding.EncodeInteger(code), bencoding.EncodeString(message)),
	}))
}

func decodeMsg(b []byte) (msg, error) {
	var m msg

	d, _, err := bencoding.ParseDictionary(string(b))
	if err != nil {
		return m, err
	}

	var ok bool
	if m.T, ok = getString(d, "t"); !ok {
		return m, errors.New("missing transaction ID")
	}
	if m.Y, ok = getString(d, "y"); !ok {
		return m, errors.New("missing message type")
	}

	switch m.Y {
	case "q":
		if m.Q, ok = getString(d, "q"); !ok {
			return m, errors.New("query has no method")
		}
		if m.A, ok = getDict(d, "a"); !ok {
			return m, errors.New("query has no arguments")
		}
	case "r":
		if m.R, ok = getDict(d, "r"); !ok {
			return m, errors.New("response has no return values")
		}
	case "e":
		list, _, err := bencoding.ParseList(d["e"])
		if err != nil || len(list) < 2 {
			return m, errors.New("malformed error")
		}
		m.ErrCode, _, _ = bencoding.ParseInteger(list[0])
		m.ErrMsg, _, _ = bencoding.ParseString(list[1])
	default:
		return m, fmt.Errorf("unknown message type %q", m.Y)
	}

	return m, nil
}

func getString(d map[string]string, key string) (string, bool) {
	raw, ok := d[key]
	if !ok {
		return "", false
	}
	s, _, err := bencoding.ParseString(raw)
	return s, err == nil
}

func getInt(d map[string]string, key string) (int, bool) {
	raw, ok := d[key]
	if !ok {
		return 0, false
	}
	n, _, err := bencoding.ParseInteger(raw)
	return n, err == nil
}

func getDict(d map[string]string, key string) (map[string]string, bool) {
	raw, ok := d[key]
	if !ok {
		return nil, false
	}
	v, _, err := bencoding.ParseDictionary(raw)
	return v, err == nil
}

func getID(d map[string]string, key string) (id.Hash, bool) {
	s, ok := getString(d, key)
	if !ok {
		return id.Hash{}, false
	}
	h, err := id.FromString(s)
	return h, err == nil
}

// node is a DHT node's ID and address.
type node struct {
	ID   id.Hash
	Addr netip.AddrPort
}

const compactNodeLen = 26

// encodeNodes returns the compact node info for IPv4 nodes: 20-byte
// ID, 4-byte IP and 2-byte port each.
func encodeNodes(nodes []node) string {
	b := make([]byte, 0, compactNodeLen*len(nodes))
	for _, n := range nodes {
		if !n.Addr.Addr().Is4() {
			continue
		}
		b = append(b, n.ID[:]...)
		b = append(b, encodePeer(n.Addr)...)
	}
	return string(b)
}

// decodeNodes parses compact node info, skipping entries with an
// unusable address.
func decodeNodes(s string) []node {
	var nodes []node
	for ; len(s) >= compactNodeLen; s = s[compactNodeLen:] {
		var n node
		copy(n.ID[:], s[:20])
		n.Addr = decodePeer(s[20:26])
		if n.Addr.Port() == 0 || n.Addr.Addr().IsUnspecified() {
			continue
		}
		nodes = append(nodes, n)
	}
	return nodes
}

func encodePeer(a netip.AddrPort) string {
	ip := a.Addr().As4()
	return string(binary.BigEndian.AppendUint16(ip[:], a.Port()))
}

func decodePeer(s string) netip.AddrPort {
	return netip.AddrPortFrom(netip.AddrFrom4([4]byte([]byte(s[:4]))), binary.BigEndian.Uint16([]byte(s[4:6])))
}

// decodeValues parses a get_peers "values" list of 6-byte compact peer
// addresses.
func decodeValues(raw string) []netip.AddrPort {
	list, _, err := bencoding.ParseList(raw)
	if err != nil {
		return nil
	}
	var peers []netip.AddrPort
	for _, item := range list {
		s, _, err := bencoding.ParseString(item)
		if err != nil || len(s) != 6 {
			continue
		}
		peers = append(peers, decodePeer(s))
	}
	return peers
}
