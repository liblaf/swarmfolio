// Package metainfo extracts the v1 info hash from a BitTorrent metainfo file.
package metainfo

import (
	"crypto/sha1" // BitTorrent v1 defines the info hash as SHA-1.
	"encoding/hex"
	"errors"
	"fmt"
)

func InfoHash(data []byte) (string, error) {
	if len(data) == 0 || data[0] != 'd' {
		return "", errors.New("metainfo: top-level value must be a dictionary")
	}
	position := 1
	var info []byte
	for position < len(data) && data[position] != 'e' {
		key, next, err := byteString(data, position)
		if err != nil {
			return "", fmt.Errorf("metainfo: dictionary key: %w", err)
		}
		position = next
		start := position
		position, err = skip(data, position, 0)
		if err != nil {
			return "", err
		}
		if string(key) == "info" {
			if info != nil {
				return "", errors.New("metainfo: duplicate info dictionary")
			}
			info = data[start:position]
		}
	}
	if position != len(data)-1 || data[position] != 'e' {
		return "", errors.New("metainfo: trailing or incomplete top-level data")
	}
	if info == nil {
		return "", errors.New("metainfo: info dictionary is missing")
	}
	sum := sha1.Sum(info)
	return hex.EncodeToString(sum[:]), nil
}

func skip(data []byte, position, depth int) (int, error) {
	if depth > 100 {
		return 0, errors.New("metainfo: bencode nesting exceeds 100 levels")
	}
	if position >= len(data) {
		return 0, errors.New("metainfo: unexpected end of bencode")
	}
	switch data[position] {
	case 'i':
		position++
		start := position
		for position < len(data) && data[position] != 'e' {
			if (data[position] < '0' || data[position] > '9') && !(position == start && data[position] == '-') {
				return 0, errors.New("metainfo: invalid integer")
			}
			position++
		}
		if position == start || position >= len(data) {
			return 0, errors.New("metainfo: incomplete integer")
		}
		return position + 1, nil
	case 'l':
		position++
		for position < len(data) && data[position] != 'e' {
			var err error
			position, err = skip(data, position, depth+1)
			if err != nil {
				return 0, err
			}
		}
		if position >= len(data) {
			return 0, errors.New("metainfo: incomplete list")
		}
		return position + 1, nil
	case 'd':
		position++
		for position < len(data) && data[position] != 'e' {
			_, next, err := byteString(data, position)
			if err != nil {
				return 0, fmt.Errorf("metainfo: dictionary key: %w", err)
			}
			position, err = skip(data, next, depth+1)
			if err != nil {
				return 0, err
			}
		}
		if position >= len(data) {
			return 0, errors.New("metainfo: incomplete dictionary")
		}
		return position + 1, nil
	default:
		_, next, err := byteString(data, position)
		return next, err
	}
}

func byteString(data []byte, position int) ([]byte, int, error) {
	if position >= len(data) || data[position] < '0' || data[position] > '9' {
		return nil, 0, errors.New("expected byte string")
	}
	length := 0
	start := position
	for position < len(data) && data[position] != ':' {
		if data[position] < '0' || data[position] > '9' {
			return nil, 0, errors.New("invalid byte string length")
		}
		if length > (len(data)-(int(data[position])-'0'))/10 {
			return nil, 0, errors.New("byte string length overflows")
		}
		length = length*10 + int(data[position]-'0')
		position++
	}
	if position >= len(data) {
		return nil, 0, errors.New("incomplete byte string length")
	}
	if position-start > 1 && data[start] == '0' {
		return nil, 0, errors.New("byte string length has a leading zero")
	}
	position++
	if length > len(data)-position {
		return nil, 0, errors.New("incomplete byte string")
	}
	return data[position : position+length], position + length, nil
}
