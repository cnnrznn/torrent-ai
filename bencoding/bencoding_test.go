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
	t.Run("parse an empty list", func(t *testing.T) {})
	t.Run("parse a list with every type", func(t *testing.T) {})
	t.Run("return an error when list does not begin with l", func(t *testing.T) {})
	t.Run("return an error when list does not end with e", func(t *testing.T) {})
}
func TestParseDictionary(t *testing.T) {

}
