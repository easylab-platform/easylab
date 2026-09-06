// Package encoding implements compact binary codecs for EasyVCS metadata and
// trees. It replaces the previous JSON-text/line-text representations, which
// dominated the SQLite database size (metadata was ~99% of the store). These
// codecs are dependency-free (standard library) and produce much smaller,
// faster-to-decode payloads.
//
// To avoid a dependency cycle (store needs encoding to serialize snapshots,
// while encoding needs store types to encode them), the encode/decode functions
// operate on primitive/alias types and return the store types only through the
// store package's own integration layer. Here we define local value types.
package encoding

import (
	"errors"
	"time"

	"easyvcs/internal/object"
)

// Author is a compact alias of the store author value (name/email).
type Author struct {
	Name  string
	Email string
}

// SnapshotMeta is the decoded compact snapshot metadata.
type SnapshotMeta struct {
	Parents     []object.ID
	Author      Author
	Description string
}

// BaseSnapshot is a minimal snapshot view used by encode (id-independent).
type BaseSnapshot struct {
	Parents     []object.ID
	Author      Author
	Description string
}

// EncodeSnapshotMeta packs parents, author, and description into a compact
// binary blob (varints + length-prefixed strings).
func EncodeSnapshotMeta(s BaseSnapshot) []byte {
	buf := make([]byte, 0, 64)
	buf = appendUvarint(buf, uint64(len(s.Parents)))
	for _, p := range s.Parents {
		buf = appendUvarint(buf, 32) // id length (constant)
		buf = append(buf, p[:]...)
	}
	buf = appendUvarint(buf, uint64(len(s.Author.Name)))
	buf = append(buf, s.Author.Name...)
	buf = appendUvarint(buf, uint64(len(s.Author.Email)))
	buf = append(buf, s.Author.Email...)
	buf = appendUvarint(buf, uint64(len(s.Description)))
	buf = append(buf, s.Description...)
	return buf
}

// DecodeSnapshotMeta is the inverse of EncodeSnapshotMeta.
func DecodeSnapshotMeta(payload []byte) (SnapshotMeta, error) {
	n, off := readUvarint(payload)
	if off == len(payload) {
		return SnapshotMeta{}, errors.New("truncated snapshot meta")
	}
	var parents []object.ID
	for i := 0; i < int(n); i++ {
		l, adv := readUvarint(payload[off:])
		off += adv
		if off+int(l) > len(payload) {
			return SnapshotMeta{}, errors.New("truncated snapshot parents")
		}
		var pid object.ID
		if int(l) > 0 {
			copy(pid[:], payload[off:off+int(l)])
		}
		off += int(l)
		parents = append(parents, pid)
	}
	// author name
	ln, adv := readUvarint(payload[off:])
	off += adv
	name := string(payload[off : off+int(ln)])
	off += int(ln)
	// author email
	ln, adv = readUvarint(payload[off:])
	off += adv
	email := string(payload[off : off+int(ln)])
	off += int(ln)
	// description
	ln, adv = readUvarint(payload[off:])
	off += adv
	desc := string(payload[off : off+int(ln)])
	off += int(ln)
	return SnapshotMeta{Parents: parents, Author: Author{Name: name, Email: email}, Description: desc}, nil
}

// EncodeChangeBody encodes a change's current snapshot id and fork_from.
func EncodeChangeBody(current object.ID, forkFrom string) []byte {
	buf := make([]byte, 0, 40)
	buf = append(buf, current[:]...)
	buf = appendUvarint(buf, uint64(len(forkFrom)))
	buf = append(buf, forkFrom...)
	return buf
}

// DecodeChangeBody decodes a change body.
func DecodeChangeBody(body []byte) (current object.ID, forkFrom string, err error) {
	if len(body) < 32 {
		return object.ID{}, "", errors.New("truncated change body")
	}
	var cur object.ID
	copy(cur[:], body[:32])
	off := 32
	ln, adv := readUvarint(body[off:])
	off += adv
	if off+int(ln) > len(body) {
		return object.ID{}, "", errors.New("truncated change fork_from")
	}
	fork := string(body[off : off+int(ln)])
	return cur, fork, nil
}

// EncodeTree binary-encodes a tree: count, then per entry (name, kind, id).
// This replaces the previous line-text format (name\nkind\nsha\n), which
// bloat for trees with many files.
func EncodeTree(t *object.Tree) []byte {
	entries := t.SortedEntries()
	size := 4
	for _, e := range entries {
		size += len(e.Name) + 1 + 32
	}
	buf := make([]byte, 0, size)
	buf = appendUvarint(buf, uint64(len(entries)))
	for _, e := range entries {
		buf = appendUvarint(buf, uint64(len(e.Name)))
		buf = append(buf, e.Name...)
		buf = append(buf, byte(e.Kind))
		buf = append(buf, e.ID[:]...)
	}
	return buf
}

// DecodeTree decodes a binary tree.
func DecodeTree(payload []byte) (*object.Tree, error) {
	n, off := readUvarint(payload)
	t := object.NewTree()
	for i := 0; i < int(n); i++ {
		ln, adv := readUvarint(payload[off:])
		off += adv
		if off+int(ln)+1+32 > len(payload) {
			return nil, errors.New("truncated tree entry")
		}
		name := string(payload[off : off+int(ln)])
		off += int(ln)
		kind := object.Kind(payload[off])
		off++
		var id object.ID
		copy(id[:], payload[off:off+32])
		off += 32
		t.Entries[name] = object.Entry{Name: name, Kind: kind, ID: id}
	}
	return t, nil
}

func appendUvarint(dst []byte, v uint64) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

func readUvarint(data []byte) (uint64, int) {
	var v uint64
	var shift uint
	var i int
	for i < len(data) {
		b := data[i]
		if b < 0x80 {
			v |= uint64(b) << shift
			return v, i + 1
		}
		v |= uint64(b&0x7f) << shift
		shift += 7
		i++
	}
	return v, len(data)
}

var _ = time.Now
