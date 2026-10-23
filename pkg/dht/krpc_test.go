package dht

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/cnnrznn/torrent-scratch/pkg/bencoding"
	"github.com/cnnrznn/torrent-scratch/pkg/id"
)

func TestKRPC(t *testing.T) {
	t.Run("decodes the BEP 5 ping example", func(t *testing.T) {
		m, err := decodeMsg([]byte("d1:ad2:id20:abcdefghij0123456789e1:q4:ping1:t2:aa1:y1:qe"))
		require.NoError(t, err)
		require.Equal(t, "aa", m.T)
		require.Equal(t, "q", m.Y)
		require.Equal(t, "ping", m.Q)
		nodeID, ok := getID(m.A, "id")
		require.True(t, ok)
		require.Equal(t, "abcdefghij0123456789", nodeID.String())
	})

	t.Run("round trips a query", func(t *testing.T) {
		b := encodeQuery("xy", "find_node", map[string]string{"target": bencoding.EncodeString("t")})
		m, err := decodeMsg(b)
		require.NoError(t, err)
		require.Equal(t, "find_node", m.Q)
		target, _ := getString(m.A, "target")
		require.Equal(t, "t", target)
	})

	t.Run("round trips a response", func(t *testing.T) {
		m, err := decodeMsg(encodeResponse("xy", map[string]string{"token": bencoding.EncodeString("tok")}))
		require.NoError(t, err)
		require.Equal(t, "r", m.Y)
		token, _ := getString(m.R, "token")
		require.Equal(t, "tok", token)
	})

	t.Run("round trips an error", func(t *testing.T) {
		m, err := decodeMsg(encodeError("xy", errMethod, "Method Unknown"))
		require.NoError(t, err)
		require.Equal(t, "e", m.Y)
		require.Equal(t, errMethod, m.ErrCode)
		require.Equal(t, "Method Unknown", m.ErrMsg)
	})

	t.Run("rejects malformed messages", func(t *testing.T) {
		for _, s := range []string{
			"garbage",
			"d1:y1:qe",                // no transaction ID
			"d1:t2:aa1:y1:qe",         // query without method
			"d1:t2:aa1:y1:re",         // response without values
			"d1:t2:aa1:y1:z1:rdee",    // unknown type
			"d1:t2:aa1:y1:e1:eli1eee", // short error
		} {
			_, err := decodeMsg([]byte(s))
			require.Error(t, err, s)
		}
	})
}

func TestCompactEncoding(t *testing.T) {
	nodes := []node{
		{ID: id.Random(), Addr: netip.MustParseAddrPort("1.2.3.4:6881")},
		{ID: id.Random(), Addr: netip.MustParseAddrPort("[::1]:6881")}, // skipped
		{ID: id.Random(), Addr: netip.MustParseAddrPort("5.6.7.8:1")},
	}
	s := encodeNodes(nodes)
	require.Len(t, s, 2*compactNodeLen)
	require.Equal(t, []node{nodes[0], nodes[2]}, decodeNodes(s))

	// A trailing partial entry and a zero port are ignored.
	bad := encodeNodes([]node{{ID: id.Random(), Addr: netip.MustParseAddrPort("1.1.1.1:0")}})
	require.Empty(t, decodeNodes(bad+"short"))

	values := bencoding.EncodeList(
		bencoding.EncodeString(encodePeer(netip.MustParseAddrPort("9.9.9.9:80"))),
		bencoding.EncodeString("bad"),
	)
	require.Equal(t, []netip.AddrPort{netip.MustParseAddrPort("9.9.9.9:80")}, decodeValues(values))
}
