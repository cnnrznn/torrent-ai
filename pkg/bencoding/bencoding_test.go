package bencoding

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseString(t *testing.T) {
	t.Run("parses well formed string", func(t *testing.T) {
		s, size, err := ParseString("4:hellgiberish")
		require.NoError(t, err)
		require.Equal(t, "hell", s)
		require.Equal(t, 6, size)
	})

	t.Run("returns error when size is invalid", func(t *testing.T) {
		s, _, err := ParseString("i4:word")
		require.ErrorContains(t, err, "invalid string size:")
		require.Empty(t, s)
	})

	t.Run("returns error when string does not contain a colon", func(t *testing.T) {
		s, _, err := ParseString("hello")
		require.EqualError(t, err, "invalid string")
		require.Empty(t, s)
	})

	t.Run("returns an error when string is shorter than specified length", func(t *testing.T) {
		s, _, err := ParseString("5:hell")
		require.EqualError(t, err, "string is shorter than length")
		require.Empty(t, s)
	})
}

func TestParseInteger(t *testing.T) {
	t.Run("parses well-formed integer", func(t *testing.T) {
		x, s, err := ParseInteger("i-1234e")
		require.NoError(t, err)
		require.Equal(t, 7, s)
		require.Equal(t, -1234, x)
	})

	t.Run("returns an error when string does not begin with i", func(t *testing.T) {
		x, s, err := ParseInteger("1234e")
		require.ErrorContains(t, err, "integer does not begin with i")
		require.Zero(t, s)
		require.Zero(t, x)
	})

	t.Run("returns an error when string does not end with e", func(t *testing.T) {
		x, s, err := ParseInteger("i1234")
		require.ErrorContains(t, err, "integer does not end with e")
		require.Zero(t, s)
		require.Zero(t, x)
	})

	t.Run("returns an error when integer is invalid", func(t *testing.T) {
		x, s, err := ParseInteger("istringe")
		require.ErrorContains(t, err, "invalid integer:")
		require.Zero(t, s)
		require.Zero(t, x)
	})
}

func TestParseList(t *testing.T) {
	t.Run("parse an empty list", func(t *testing.T) {
		ls, size, err := ParseList("le")
		require.NoError(t, err)
		require.Equal(t, 2, size)
		require.Empty(t, ls)
	})
	t.Run("parse list with two strings", func(t *testing.T) {
		ls, size, err := ParseList("l5:hello5:worlde")
		require.NoError(t, err)
		require.Equal(t, 16, size)
		require.Equal(t, []string{"5:hello", "5:world"}, ls)
	})
	t.Run("parse list with list", func(t *testing.T) {
		ls, size, err := ParseList("ll4:hellee")
		require.NoError(t, err)
		require.Equal(t, 10, size)
		require.Equal(t, []string{"l4:helle"}, ls)
	})
	t.Run("parse list with a dictionary", func(t *testing.T) {
		ls, size, err := ParseList("ldee")
		require.NoError(t, err)
		require.Equal(t, 4, size)
		require.Equal(t, []string{"de"}, ls)
	})
	t.Run("parse a list with every type", func(t *testing.T) {
		ls, size, err := ParseList("l4:helli69420el6:nesteded3:key5:valueee")
		require.NoError(t, err)
		require.Equal(t, 39, size)
		require.Equal(t, []string{"4:hell", "i69420e", "l6:nestede", "d3:key5:valuee"}, ls)
	})
	t.Run("return an error when list does not begin with l", func(t *testing.T) {
		ls, size, err := ParseList("5:helloe")
		require.ErrorContains(t, err, "invalid list")
		require.Equal(t, 0, size)
		require.Empty(t, ls)
	})
	t.Run("return an error when list does not end with e", func(t *testing.T) {
		ls, size, err := ParseList("l5:hello")
		require.ErrorContains(t, err, "EOF parsing list")
		require.Equal(t, 0, size)
		require.Empty(t, ls)
	})
}
func TestParseDictionary(t *testing.T) {
	t.Run("successfully parse dictionary", func(t *testing.T) {
		d, size, err := ParseDictionary("d3:key5:value4:key26:value2e")
		require.NoError(t, err)
		require.Equal(t, 28, size)
		require.Equal(t, map[string]string{
			"key":  "5:value",
			"key2": "6:value2",
		}, d)
	})
}

func TestParseEmptyAndTruncated(t *testing.T) {
	t.Run("empty input returns errors", func(t *testing.T) {
		_, _, err := ParseString("")
		require.Error(t, err)
		_, _, err = ParseInteger("")
		require.Error(t, err)
		_, _, err = ParseList("")
		require.Error(t, err)
		_, _, err = ParseDictionary("")
		require.Error(t, err)
	})

	t.Run("truncated list returns an error", func(t *testing.T) {
		_, _, err := ParseList("l")
		require.Error(t, err)
	})

	t.Run("truncated dictionary returns an error", func(t *testing.T) {
		_, _, err := ParseDictionary("d3:foo")
		require.Error(t, err)
		_, _, err = ParseDictionary("d3:fooi1e")
		require.ErrorContains(t, err, "EOF parsing dictionary")
	})

	t.Run("huge string length returns an error", func(t *testing.T) {
		_, _, err := ParseString("9223372036854775807:a")
		require.ErrorContains(t, err, "string is shorter than length")
	})

	t.Run("negative string length returns an error", func(t *testing.T) {
		_, _, err := ParseString("-1:a")
		require.ErrorContains(t, err, "invalid string size:")
	})
}

func TestParseIntegerCanonical(t *testing.T) {
	for _, s := range []string{"ie", "i-0e", "i03e", "i-03e", "i+3e", "i-e"} {
		t.Run("rejects "+s, func(t *testing.T) {
			_, _, err := ParseInteger(s)
			require.ErrorContains(t, err, "invalid integer:")
		})
	}

	t.Run("accepts zero", func(t *testing.T) {
		x, _, err := ParseInteger("i0e")
		require.NoError(t, err)
		require.Zero(t, x)
	})
}

func TestEncode(t *testing.T) {
	t.Run("encodes a string", func(t *testing.T) {
		require.Equal(t, "5:hello", EncodeString("hello"))
		require.Equal(t, "0:", EncodeString(""))
	})

	t.Run("encodes an integer", func(t *testing.T) {
		require.Equal(t, "i42e", EncodeInteger(42))
		require.Equal(t, "i-3e", EncodeInteger(-3))
		require.Equal(t, "i0e", EncodeInteger(0))
	})

	t.Run("encodes a list", func(t *testing.T) {
		require.Equal(t, "le", EncodeList())
		require.Equal(t, "l4:spami7ee", EncodeList(EncodeString("spam"), EncodeInteger(7)))
	})

	t.Run("encodes a dictionary with sorted keys", func(t *testing.T) {
		d := EncodeDictionary(map[string]string{
			"zebra": EncodeInteger(1),
			"apple": EncodeString("x"),
		})
		require.Equal(t, "d5:apple1:x5:zebrai1ee", d)
	})

	t.Run("round trips through the parser", func(t *testing.T) {
		in := map[string]string{
			"a": EncodeString("hello"),
			"b": EncodeList(EncodeInteger(1), EncodeDictionary(map[string]string{})),
		}
		d, size, err := ParseDictionary(EncodeDictionary(in))
		require.NoError(t, err)
		require.Equal(t, len(EncodeDictionary(in)), size)
		require.Equal(t, in, d)
	})
}

func FuzzParseDictionary(f *testing.F) {
	f.Add("d3:key5:value4:key26:value2e")
	f.Add("l4:helli69420el6:nesteded3:key5:valueee")
	f.Add("d1:ad1:bli1ei-2eee")
	f.Fuzz(func(t *testing.T, s string) {
		ParseDictionary(s)
		ParseList(s)
		ParseInteger(s)
		ParseString(s)
	})
}
