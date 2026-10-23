package bencoding

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type Type int

const (
	TypeString = iota
	TypeInteger
	TypeList
	TypeDictionary
)

// Parse finds the size of a bencoded string. It
// returns an error if the string is malformed.
func ParseString(s string) (string, int, error) {
	i := strings.Index(s, ":")
	if i <= 0 {
		return "", 0, errors.New("invalid string")
	}

	size, err := parseDecimal(s[:i], false)
	if err != nil {
		return "", 0, fmt.Errorf("invalid string size: %w", err)
	}

	start := i + 1
	if size > len(s)-start {
		return "", 0, errors.New("string is shorter than length")
	}
	end := start + size

	return s[start:end], end, nil
}

// LenInteger finds the size of a bencoded integer.
func ParseInteger(s string) (int, int, error) {
	if len(s) == 0 || s[0] != 'i' {
		return 0, 0, errors.New("integer does not begin with i")
	}

	i := strings.Index(s, "e")
	if i < 0 {
		return 0, 0, errors.New("integer does not end with e")
	}

	end := i + 1

	n, err := parseDecimal(s[1:i], true)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid integer: %w", err)
	}

	return n, end, nil
}

func ParseList(s string) ([]string, int, error) {
	if len(s) == 0 || s[0] != 'l' {
		return nil, 0, errors.New("invalid list")
	}

	result := []string{}

	var err error
	var size int

	// loop over the buffer, getting the length of each next element.
	// add the length of the element to end every iteration
	var end int
	for end = 1; end < len(s) && s[end] != 'e'; {
		switch typeNext(s[end:]) {
		case TypeString:
			_, size, err = ParseString(s[end:])
		case TypeInteger:
			_, size, err = ParseInteger(s[end:])
		case TypeList:
			_, size, err = ParseList(s[end:])
		case TypeDictionary:
			_, size, err = ParseDictionary(s[end:])
		}
		if err != nil {
			return nil, 0, fmt.Errorf("error parsing list item: %w", err)
		}
		result = append(result, s[end:end+size])
		end += size
	}

	if end >= len(s) {
		return nil, 0, errors.New("EOF parsing list")
	}
	if s[end] != 'e' {
		return nil, 0, errors.New("list does not end in e")
	}

	return result, end + 1, nil
}

func ParseDictionary(s string) (map[string]string, int, error) {
	// A dictionary is a string of the format <d><content><e>
	// where <content> is a variable size list of <string><value> bencoded
	// elements.
	//
	// Parse a string, then get the type and size of the next element, and increment the pointer.
	// Rinse and repeat to build a map of string->string
	result := map[string]string{}

	if len(s) == 0 || s[0] != 'd' {
		return nil, 0, errors.New("dictionary does not begin with d")
	}

	var end int
	for end = 1; end < len(s) && s[end] != 'e'; {
		// Parse key
		key, size, err := ParseString(s[end:])
		if err != nil {
			return nil, 0, fmt.Errorf("invalid key: %w", err)
		}

		end += size

		// Parse value
		switch typeNext(s[end:]) {
		case TypeString:
			_, size, err = ParseString(s[end:])
		case TypeList:
			_, size, err = ParseList(s[end:])
		case TypeInteger:
			_, size, err = ParseInteger(s[end:])
		case TypeDictionary:
			_, size, err = ParseDictionary(s[end:])
		}

		if err != nil {
			return nil, 0, fmt.Errorf("malformed dictionary value: %w", err)
		}

		result[key] = s[end : end+size]
		end += size
	}

	if end >= len(s) {
		return nil, 0, errors.New("EOF parsing dictionary")
	}
	if s[end] != 'e' {
		return nil, 0, errors.New("dictionary does not end with e")
	}

	return result, end + 1, nil
}

// parseDecimal parses a base-10 integer in the canonical form
// required by BEP 3: no sign other than a leading '-', no leading
// zeros, and no negative zero.
func parseDecimal(s string, allowNegative bool) (int, error) {
	digits := s
	if allowNegative && strings.HasPrefix(digits, "-") {
		digits = digits[1:]
		if digits == "0" {
			return 0, errors.New("negative zero")
		}
	}

	if len(digits) == 0 {
		return 0, errors.New("no digits")
	}
	if len(digits) > 1 && digits[0] == '0' {
		return 0, errors.New("leading zero")
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, fmt.Errorf("invalid digit %q", digits[i])
		}
	}

	return strconv.Atoi(s)
}

func typeNext(s string) Type {
	if len(s) == 0 {
		return TypeString
	}

	switch s[0] {
	case 'l':
		return TypeList
	case 'd':
		return TypeDictionary
	case 'i':
		return TypeInteger
	default:
		return TypeString
	}
}
