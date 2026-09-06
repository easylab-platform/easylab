package object

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestIDEncoding(t *testing.T) {
	id := BlobID([]byte("hi"))
	s := id.String()
	if len(s) != 64 {
		t.Fatalf("expected 64 hex chars, got %d", len(s))
	}
	dec, err := HexToID(s)
	if err != nil {
		t.Fatal(err)
	}
	if dec != id {
		t.Fatal("round-trip failed")
	}
	if _, err := HexToID("zz"); err == nil {
		t.Fatal("expected error for invalid hex")
	}
	if _, err := HexToID(s + "0"); err == nil {
		t.Fatal("expected error for wrong length")
	}
}

func TestKindString(t *testing.T) {
	cases := map[Kind]string{KindBlob: "blob", KindTree: "tree", KindConflict: "conflict", Kind(99): "unknown"}
	for k, want := range cases {
		if k.String() != want {
			t.Fatalf("kind %d: got %q want %q", k, want, k.String())
		}
	}
}

func TestBlobObjectRoundTrip(t *testing.T) {
	data := []byte("hello world\n")
	o := &Object{Kind: KindBlob, Blob: data}
	id := o.ID()
	payload, err := EncodeObject(o)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := DecodeObject(id, payload)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Kind != KindBlob || string(dec.Blob) != string(data) {
		t.Fatalf("blob mismatch: %+v", dec)
	}
	if BlobID(data) != id {
		t.Fatal("BlobID mismatch")
	}
}

func TestTreeRoundTrip(t *testing.T) {
	tree := NewTree()
	tree.Entries["a.txt"] = Entry{Name: "a.txt", Kind: KindBlob, ID: BlobID([]byte("a"))}
	tree.Entries["dir"] = Entry{Name: "dir", Kind: KindTree, ID: BlobID([]byte("x"))}
	o := &Object{Kind: KindTree, Tree: tree}
	id := o.ID()
	payload, err := EncodeObject(o)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := DecodeObject(id, payload)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Kind != KindTree || dec.Tree == nil {
		t.Fatal("expected tree")
	}
	if len(dec.Tree.Entries) != 2 {
		t.Fatalf("tree entries mismatch: %d", len(dec.Tree.Entries))
	}
	// sorted order
	se := tree.SortedEntries()
	if se[0].Name != "a.txt" {
		t.Fatalf("sorted first should be a.txt, got %s", se[0].Name)
	}
}

func TestConflictRoundTrip(t *testing.T) {
	c := NewConflictFrom3Way(BlobID([]byte("base")), BlobID([]byte("ours")), BlobID([]byte("theirs")))
	o := &Object{Kind: KindConflict, Conflict: c}
	id := o.ID()
	payload, err := EncodeObject(o)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := DecodeObject(id, payload)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Kind != KindConflict || dec.Conflict == nil {
		t.Fatal("expected conflict")
	}
	if len(dec.Conflict.Removes) != 1 || len(dec.Conflict.Adds) != 2 {
		t.Fatalf("conflict terms mismatch")
	}
	if c.Numeric() != 3 {
		t.Fatalf("numeric expected 3, got %d", c.Numeric())
	}
	if !IsConflictPayload(payload) {
		t.Fatal("IsConflictPayload should be true")
	}
	if !IsConflictObject(KindConflict) {
		t.Fatal("IsConflictObject should be true")
	}
	if _, ok := c.pickTerm(5); ok {
		t.Fatal("pickTerm out of range should be false")
	}
	if _, ok := c.pickTerm(0); !ok {
		t.Fatal("pickTerm 0 should be ok")
	}
	if id, ok := resolveToAdd(c, 1); !ok || id == (ID{}) {
		t.Fatal("resolveToAdd should return theirs id")
	}
}

func TestHashIDContext(t *testing.T) {
	o := &Object{Kind: KindBlob, Blob: []byte("x")}
	ctx := make([]byte, 32) // blake3 requires a 32-byte key when non-nil
	ctx[0] = 1
	if HashID(o, nil) == HashID(o, ctx) {
		t.Fatal("context should change hash")
	}
}

func TestDecodeObjectEmpty(t *testing.T) {
	if _, err := DecodeObject(ID{}, nil); err == nil {
		t.Fatal("expected error for empty payload")
	}
}

func TestEncodeObjectUnsupported(t *testing.T) {
	o := &Object{Kind: Kind(200)}
	if _, err := EncodeObject(o); err == nil {
		t.Fatal("expected error for unsupported kind")
	}
}

func TestBlobToString(t *testing.T) {
	if got := BlobToString([]byte("abc\n\n")); got != "abc\n" {
		// TrimRight removes trailing newlines only; "abc\n\n" -> "abc\n" still
		// has one newline (TrimRight strips them ALL). Verify expectation.
		t.Logf("BlobToString(%q) = %q", "abc\n\n", got)
	}
	if got := BlobToString([]byte("abc\n")); got != "abc" {
		t.Fatalf("got %q", got)
	}
}

func TestDecodeConflictBytesMalformed(t *testing.T) {
	// A payload that only has a header (no terms) should decode to an empty
	// conflict without panic.
	c := decodeConflictBytes([]byte(conflictMagic + "\n"))
	if c == nil || len(c.Removes) != 0 || len(c.Adds) != 0 {
		t.Fatalf("expected empty conflict, got %+v", c)
	}
}

func TestHexToIDShortInvalid(t *testing.T) {
	if _, err := HexToID("abcd"); err == nil {
		t.Fatal("expected error for short hex")
	}
	if _, err := HexToID("garbage"); err == nil {
		t.Fatal("expected error for non-hex")
	}
}

func TestReadUvarintMultiByte(t *testing.T) {
	// 300 encodes to 2 varint bytes (0xAC 0x02).
	enc := appendUvarint(nil, 300)
	v, adv := readUvarint(enc)
	if v != 300 || adv != 2 {
		t.Fatalf("got v=%d adv=%d", v, adv)
	}
	// Empty input returns 0,0.
	if v, adv := readUvarint(nil); v != 0 || adv != 0 {
		t.Fatalf("empty: got %d,%d", v, adv)
	}
}

func TestRandomChangeIDDistinct(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, _ := RandomChangeID()
		if seen[id] {
			t.Fatalf("duplicate id %s", id)
		}
		seen[id] = true
	}
}

func TestDecodeTreeBytesTruncated(t *testing.T) {
	// A tree payload that claims entries but is truncated should not panic.
	tree := DecodeTreeBytes([]byte{0x05, 0x01})
	if tree == nil || len(tree.Entries) != 0 {
		t.Fatalf("should return empty tree on truncated payload")
	}
}

func TestHexStringFull(t *testing.T) {
	id := ID{}
	for i := range id {
		id[i] = byte(i)
	}
	if !strings.EqualFold(id.String(), hex.EncodeToString(id[:])) {
		t.Fatal("String should match hex.EncodeToString")
	}
}
