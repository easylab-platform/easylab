// Package object implements content-addressed immutable objects.
//
// Objects are identified by a BLAKE3 hash of their encoded form. This gives
// content addressing: identical content always maps to the same id, so objects
// can be deduplicated and stored immutably. An object is either a Blob (file
// bytes), a Tree (a directory listing of named entries), or a Conflict.
package object

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"lukechampine.com/blake3"
)

// ID is a content-addressed object identifier (32-byte BLAKE3 digest).
type ID [32]byte

// String returns the lowercase hex encoding of the ID.
func (o ID) String() string { return hex.EncodeToString(o[:]) }

// IsZero reports whether the ID is the zero value (no object).
func (o ID) IsZero() bool { return o == ID{} }

// HexToID decodes a hex string into an ID.
func HexToID(s string) (ID, error) {
	var id ID
	b, err := hex.DecodeString(s)
	if err != nil {
		return id, err
	}
	if len(b) != len(id) {
		return id, fmt.Errorf("invalid object id length %d", len(b))
	}
	copy(id[:], b)
	return id, nil
}

const hashLen = 32

// Kind discriminates the type of an object.
type Kind uint8

// Object kinds.
const (
	KindBlob Kind = iota
	KindTree
	KindConflict
)

func (k Kind) String() string {
	switch k {
	case KindBlob:
		return "blob"
	case KindTree:
		return "tree"
	case KindConflict:
		return "conflict"
	default:
		return "unknown"
	}
}

// Object is a decoded immutable object.
type Object struct {
	Kind     Kind
	Blob     []byte
	Tree     *Tree
	Conflict *Conflict
}

// ID computes and returns the content-addressed id of the object.
func (o *Object) ID() ID {
	return HashID(o, nil)
}

// HashID computes the content id of an object. The optional context is mixed
// into the hash so callers can namespace hashes if needed.
func HashID(o *Object, context []byte) ID {
	h := blake3.New(hashLen, context)
	fmt.Fprintf(h, "%d\n", o.Kind)
	switch o.Kind {
	case KindBlob:
		fmt.Fprintf(h, "%d\n", len(o.Blob))
		h.Write(o.Blob)
	case KindTree:
		o.Tree.encoded(h)
	case KindConflict:
		fmt.Fprintf(h, "%d\n", len(o.Conflict.Removes))
		for _, t := range o.Conflict.Removes {
			fmt.Fprintf(h, "%s\n%s\n", hex.EncodeToString(t.ID[:]), t.Label)
		}
		fmt.Fprintf(h, "%d\n", len(o.Conflict.Adds))
		for _, t := range o.Conflict.Adds {
			fmt.Fprintf(h, "%s\n%s\n", hex.EncodeToString(t.ID[:]), t.Label)
		}
	}
	var out ID
	h.Sum(out[:0])
	return out
}

// Tree is a directory: a sorted map of entry name -> entry.
type Tree struct {
	Entries map[string]Entry
}

// Entry is a named child object inside a Tree.
type Entry struct {
	Name string
	ID   ID
	Kind Kind
}

// NewTree returns an empty tree.
func NewTree() *Tree { return &Tree{Entries: map[string]Entry{}} }

// ID returns the content-addressed id of this tree.
func (t *Tree) ID() ID { return HashID(&Object{Kind: KindTree, Tree: t}, nil) }

// SortedEntries returns the entries in lexicographical order.
func (t *Tree) SortedEntries() []Entry {
	names := make([]string, 0, len(t.Entries))
	for n := range t.Entries {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]Entry, 0, len(names))
	for _, n := range names {
		out = append(out, t.Entries[n])
	}
	return out
}

func (t *Tree) encoded(h interface{ Write([]byte) (int, error) }) {
	fmt.Fprintf(h, "%d\n", len(t.Entries))
	for _, e := range t.SortedEntries() {
		fmt.Fprintf(h, "%s\n%d\n%s\n", e.Name, e.Kind, hex.EncodeToString(e.ID[:]))
	}
}

// BlobID computes the content id of raw bytes.
func BlobID(data []byte) ID {
	return HashID(&Object{Kind: KindBlob, Blob: data}, nil)
}

// EncodeObject serializes an object into its canonical byte form used for
// storage. It is the inverse of DecodeObject. Trees use the compact binary
// encoding; blob content is raw bytes.
func EncodeObject(o *Object) ([]byte, error) {
	switch o.Kind {
	case KindBlob:
		return o.Blob, nil
	case KindTree:
		return EncodeTreeBytes(o.Tree), nil
	case KindConflict:
		return encodeConflictBytes(o.Conflict), nil
	default:
		return nil, fmt.Errorf("unsupported object kind %v", o.Kind)
	}
}

// DecodeObject parses the canonical byte form back into an Object. id is the
// content id the caller expects; it is recorded on the object so callers can
// sanity check. Trees are recognized by the presence of the compact binary
// encoding (whose first field is a varint); blob content is passed through.
func DecodeObject(id ID, payload []byte) (*Object, error) {
	if len(payload) == 0 {
		return nil, fmt.Errorf("empty object payload for %s", id)
	}
	// Detect known magic headers in order: conflict first, then tree.
	p := string(payload)
	if strings.HasPrefix(p, conflictMagic) {
		return &Object{Kind: KindConflict, Conflict: decodeConflictBytes(payload)}, nil
	}
	if strings.HasPrefix(p, treeMagic) {
		return &Object{Kind: KindTree, Tree: DecodeTreeBytes(payload[len(treeMagic):])}, nil
	}
	return &Object{Kind: KindBlob, Blob: payload}, nil
}

const treeMagic = "easyvcs-tree-v1"

// EncodeTreeBytes binary-encodes a tree (compact: varint count + per-entry
// name/kind/id). It replaces the old line-text form which bloat for trees with
// many files. The encoded payload is prefixed with the treeMagic marker so a
// blob can be distinguished from a tree without ambiguity.
func EncodeTreeBytes(t *Tree) []byte {
	entries := t.SortedEntries()
	size := 4 + len(treeMagic)
	for _, e := range entries {
		size += len(e.Name) + 1 + 32
	}
	buf := make([]byte, 0, size)
	buf = append(buf, treeMagic...)
	buf = appendUvarint(buf, uint64(len(entries)))
	for _, e := range entries {
		buf = appendUvarint(buf, uint64(len(e.Name)))
		buf = append(buf, e.Name...)
		buf = append(buf, byte(e.Kind))
		buf = append(buf, e.ID[:]...)
	}
	return buf
}

// DecodeTreeBytes decodes a binary tree. It expects the payload WITHOUT the
// treeMagic prefix (callers strip it after detection).
func DecodeTreeBytes(payload []byte) *Tree {
	t := NewTree()
	n, off := readUvarint(payload)
	for i := 0; i < int(n); i++ {
		ln, adv := readUvarint(payload[off:])
		off += adv
		if off+int(ln)+1+32 > len(payload) {
			return t
		}
		name := string(payload[off : off+int(ln)])
		off += int(ln)
		kind := Kind(payload[off])
		off++
		var id ID
		copy(id[:], payload[off:off+32])
		off += 32
		t.Entries[name] = Entry{Name: name, Kind: kind, ID: id}
	}
	return t
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

// BlobToString is a small helper used for debug rendering of blob contents.
func BlobToString(data []byte) string {
	return strings.TrimRight(string(data), "\n")
}

// RandomChangeID returns a random 16-byte change id encoded as 32 hex chars.
// Change ids are generated once and never change (content-independent), unlike
// object ids which are derived from content.
func RandomChangeID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
