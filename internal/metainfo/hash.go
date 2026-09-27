// Package metainfo inspects BitTorrent v1, v2, and hybrid metainfo files.
package metainfo

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
)

const (
	maxDepth = 100
	maxNodes = 100_000
)

// Info is qBittorrent's `hash` identity and the torrent payload size.
type Info struct {
	Hash string
	Size int64
}

// Inspect derives qBittorrent's torrent ID and total payload size. qBittorrent
// uses the first 20 bytes of the v2 SHA-256 infohash for v2 and hybrid torrents.
func Inspect(data []byte) (Info, error) {
	p := parser{data: data}
	root, err := p.value(0)
	if err != nil {
		return Info{}, fmt.Errorf("metainfo: %w", err)
	}
	if p.pos != len(data) {
		return Info{}, errors.New("metainfo: trailing data")
	}
	if root.kind != dictionary {
		return Info{}, errors.New("metainfo: top-level value must be a dictionary")
	}
	info, ok, err := root.field("info")
	if err != nil {
		return Info{}, err
	}
	if !ok || info.kind != dictionary {
		return Info{}, errors.New("metainfo: info dictionary is missing or invalid")
	}
	version, v2, err := info.metaVersion()
	if err != nil {
		return Info{}, err
	}
	var size int64
	if v2 {
		if version != 2 {
			return Info{}, fmt.Errorf("metainfo: unsupported meta version %d", version)
		}
		size, err = info.v2Size()
	} else {
		size, err = info.v1Size()
	}
	if err != nil {
		return Info{}, err
	}
	if v2 {
		sum := sha256.Sum256(data[info.start:info.end])
		return Info{Hash: hex.EncodeToString(sum[:20]), Size: size}, nil
	}
	sum := sha1.Sum(data[info.start:info.end])
	return Info{Hash: hex.EncodeToString(sum[:]), Size: size}, nil
}

// InfoHash returns qBittorrent's torrent ID. Use Inspect when size is needed.
func InfoHash(data []byte) (string, error) { info, err := Inspect(data); return info.Hash, err }

type kind byte

const (
	integer    kind = 'i'
	list       kind = 'l'
	dictionary kind = 'd'
	bytestring kind = 's'
)

type node struct {
	kind       kind
	start, end int
	int64      int64
	bytes      []byte
	list       []node
	dict       []entry
}
type entry struct {
	key   string
	value node
}
type parser struct {
	data  []byte
	pos   int
	nodes int
}

func (p *parser) value(depth int) (node, error) {
	if depth > maxDepth {
		return node{}, errors.New("bencode nesting exceeds 100 levels")
	}
	if p.pos >= len(p.data) {
		return node{}, errors.New("unexpected end of bencode")
	}
	p.nodes++
	if p.nodes > maxNodes {
		return node{}, errors.New("bencode has more than 100000 values")
	}
	start := p.pos
	switch p.data[p.pos] {
	case 'i':
		v, err := p.integer()
		if err != nil {
			return node{}, err
		}
		return node{kind: integer, start: start, end: p.pos, int64: v}, nil
	case 'l':
		p.pos++
		values := []node{}
		for p.pos < len(p.data) && p.data[p.pos] != 'e' {
			v, err := p.value(depth + 1)
			if err != nil {
				return node{}, err
			}
			values = append(values, v)
		}
		if p.pos >= len(p.data) {
			return node{}, errors.New("unterminated list")
		}
		p.pos++
		return node{kind: list, start: start, end: p.pos, list: values}, nil
	case 'd':
		p.pos++
		entries := []entry{}
		for p.pos < len(p.data) && p.data[p.pos] != 'e' {
			key, err := p.string()
			if err != nil {
				return node{}, fmt.Errorf("dictionary key: %w", err)
			}
			v, err := p.value(depth + 1)
			if err != nil {
				return node{}, err
			}
			entries = append(entries, entry{string(key), v})
		}
		if p.pos >= len(p.data) {
			return node{}, errors.New("unterminated dictionary")
		}
		p.pos++
		return node{kind: dictionary, start: start, end: p.pos, dict: entries}, nil
	default:
		v, err := p.string()
		if err != nil {
			return node{}, err
		}
		return node{kind: bytestring, start: start, end: p.pos, bytes: v}, nil
	}
}

func (p *parser) integer() (int64, error) {
	p.pos++
	start := p.pos
	negative := p.pos < len(p.data) && p.data[p.pos] == '-'
	if negative {
		p.pos++
	}
	digits := p.pos
	for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
		p.pos++
	}
	if digits == p.pos || p.pos >= len(p.data) || p.data[p.pos] != 'e' {
		return 0, errors.New("invalid integer")
	}
	text := p.data[start:p.pos]
	if (negative && len(text) == 2 && text[1] == '0') || (!negative && len(text) > 1 && text[0] == '0') || (negative && len(text) > 2 && text[1] == '0') {
		return 0, errors.New("non-canonical integer")
	}
	var v int64
	for _, ch := range text {
		if ch == '-' {
			continue
		}
		d := int64(ch - '0')
		if v > (math.MaxInt64-d)/10 {
			return 0, errors.New("integer overflows int64")
		}
		v = v*10 + d
	}
	if negative {
		v = -v
	}
	p.pos++
	return v, nil
}

func (p *parser) string() ([]byte, error) {
	start := p.pos
	if p.pos >= len(p.data) || p.data[p.pos] < '0' || p.data[p.pos] > '9' {
		return nil, errors.New("expected byte string")
	}
	length := 0
	for p.pos < len(p.data) && p.data[p.pos] != ':' {
		if p.data[p.pos] < '0' || p.data[p.pos] > '9' {
			return nil, errors.New("invalid byte string length")
		}
		d := int(p.data[p.pos] - '0')
		if length > (len(p.data)-d)/10 {
			return nil, errors.New("byte string length overflows")
		}
		length = length*10 + d
		p.pos++
	}
	if p.pos >= len(p.data) {
		return nil, errors.New("incomplete byte string length")
	}
	if p.pos-start > 1 && p.data[start] == '0' {
		return nil, errors.New("non-canonical byte string length")
	}
	p.pos++
	if length > len(p.data)-p.pos {
		return nil, errors.New("truncated byte string")
	}
	v := p.data[p.pos : p.pos+length]
	p.pos += length
	return v, nil
}

func (n node) field(key string) (node, bool, error) {
	var result node
	found := false
	for _, entry := range n.dict {
		if entry.key == key {
			if found {
				return node{}, false, fmt.Errorf("metainfo: duplicate %q key", key)
			}
			result, found = entry.value, true
		}
	}
	return result, found, nil
}
func (n node) metaVersion() (int64, bool, error) {
	v, ok, err := n.field("meta version")
	if err != nil || !ok {
		return 0, ok, err
	}
	if v.kind != integer {
		return 0, true, errors.New("metainfo: meta version must be an integer")
	}
	return v.int64, true, nil
}

func (n node) v1Size() (int64, error) {
	length, hasLength, err := n.field("length")
	if err != nil {
		return 0, err
	}
	files, hasFiles, err := n.field("files")
	if err != nil {
		return 0, err
	}
	if hasLength == hasFiles {
		return 0, errors.New("metainfo: v1 info must contain exactly one of length or files")
	}
	if hasLength {
		if length.kind != integer || length.int64 < 0 {
			return 0, errors.New("metainfo: v1 length must be a non-negative integer")
		}
		return length.int64, nil
	}
	if files.kind != list || len(files.list) == 0 {
		return 0, errors.New("metainfo: v1 files must be a nonempty list")
	}
	var total int64
	for _, file := range files.list {
		if file.kind != dictionary {
			return 0, errors.New("metainfo: v1 file must be a dictionary")
		}
		length, ok, err := file.field("length")
		if err != nil || !ok || length.kind != integer || length.int64 < 0 {
			return 0, errors.New("metainfo: v1 file length must be a non-negative integer")
		}
		padding, err := file.isPadding()
		if err != nil {
			return 0, err
		}
		if padding {
			continue
		}
		if total > math.MaxInt64-length.int64 {
			return 0, errors.New("metainfo: payload size overflows int64")
		}
		total += length.int64
	}
	return total, nil
}

func (n node) v2Size() (int64, error) {
	tree, ok, err := n.field("file tree")
	if err != nil || !ok || tree.kind != dictionary || len(tree.dict) == 0 {
		return 0, errors.New("metainfo: v2 info must contain a nonempty file tree")
	}
	return tree.fileTreeSize()
}
func (n node) fileTreeSize() (int64, error) {
	var total int64
	for _, entry := range n.dict {
		if entry.key == "" {
			if len(n.dict) != 1 || entry.value.kind != dictionary {
				return 0, errors.New("metainfo: invalid v2 file tree leaf")
			}
			length, ok, err := entry.value.field("length")
			if err != nil || !ok || length.kind != integer || length.int64 < 0 {
				return 0, errors.New("metainfo: v2 file length must be a non-negative integer")
			}
			padding, err := entry.value.isPadding()
			if err != nil {
				return 0, err
			}
			if padding {
				return 0, nil
			}
			return length.int64, nil
		}
		if entry.value.kind != dictionary {
			return 0, errors.New("metainfo: invalid v2 file tree directory")
		}
		size, err := entry.value.fileTreeSize()
		if err != nil {
			return 0, err
		}
		if total > math.MaxInt64-size {
			return 0, errors.New("metainfo: payload size overflows int64")
		}
		total += size
	}
	return total, nil
}

func (n node) isPadding() (bool, error) {
	attr, ok, err := n.field("attr")
	if err != nil || !ok {
		return false, err
	}
	if attr.kind != bytestring {
		return false, errors.New("metainfo: file attr must be a byte string")
	}
	return bytes.ContainsRune(attr.bytes, 'p'), nil
}
