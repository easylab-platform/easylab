package encoding

import (
	"testing"

	"easyvcs/internal/object"
)

func TestSnapshotMetaRoundTrip(t *testing.T) {
	parents := []object.ID{object.BlobID([]byte("p1")), object.BlobID([]byte("p2"))}
	bs := BaseSnapshot{
		Parents:     parents,
		Author:      Author{Name: "Alice <a@b>", Email: "a@b"},
		Description: "my commit message\nsecond line",
	}
	payload := EncodeSnapshotMeta(bs)
	dec, err := DecodeSnapshotMeta(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(dec.Parents) != 2 || dec.Parents[0] != parents[0] || dec.Parents[1] != parents[1] {
		t.Fatalf("parents mismatch: %v", dec.Parents)
	}
	if dec.Author.Name != bs.Author.Name || dec.Author.Email != bs.Author.Email {
		t.Fatalf("author mismatch: %+v", dec.Author)
	}
	if dec.Description != bs.Description {
		t.Fatalf("description mismatch: %q", dec.Description)
	}
}

func TestSnapshotMetaTruncated(t *testing.T) {
	if _, err := DecodeSnapshotMeta([]byte{}); err == nil {
		t.Fatal("expected error for empty")
	}
	// A payload claiming 1 parent but with no bytes after the count.
	if _, err := DecodeSnapshotMeta([]byte{0x01}); err == nil {
		t.Fatal("expected error for truncated parent")
	}
}

func TestEncodeDecodeSnapshotMetaEmpty(t *testing.T) {
	payload := EncodeSnapshotMeta(BaseSnapshot{})
	dec, err := DecodeSnapshotMeta(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(dec.Parents) != 0 || dec.Author != (Author{}) || dec.Description != "" {
		t.Fatalf("empty meta mismatch: %+v", dec)
	}
}

func TestSnapshotMetaCompletelyShortPayload(t *testing.T) {
	// A payload that is exactly one byte (count=1) but no other fields known.
	// After the count byte `off == len(payload)` should trigger truncated error.
	if _, err := DecodeSnapshotMeta([]byte{0x01}); err == nil {
		t.Fatal("expected error for trailing-only count (len check via off==len)")
	}
}

func TestSnapshotMetaParentLenExceedsRemaining(t *testing.T) {
	// count=1 (0x01), then parent length 0x03 which claims 3 bytes but payload
	// has none after it -> line 68 truncated parents.
	payload := []byte{0x01, 0x03}
	if _, err := DecodeSnapshotMeta(payload); err == nil {
		t.Fatal("expected error for parent length exceeding remaining bytes")
	}
}

func TestChangeBodyRoundTrip(t *testing.T) {
	cur := object.BlobID([]byte("snapshot"))
	// Double varint-length fork_from ("hi\x00world" has utf8). Use unicode to
	// exercise multi-byte.
	fork := "forked✓"
	body := EncodeChangeBody(cur, fork)
	gotCur, gotFork, err := DecodeChangeBody(body)
	if err != nil {
		t.Fatal(err)
	}
	if gotCur != cur || gotFork != fork {
		t.Fatalf("mismatch: cur=%v fork=%q", gotCur, gotFork)
	}
}

func TestChangeBodyTruncated(t *testing.T) {
	if _, _, err := DecodeChangeBody([]byte{}); err == nil {
		t.Fatal("expected error for empty body")
	}
	// 32-byte current, fork len > remaining.
	body := make([]byte, 32)
	body = append(body, 0x03) // fork len = 3, no bytes follow
	if _, _, err := DecodeChangeBody(body); err == nil {
		t.Fatal("expected error for truncated fork")
	}
}

func TestTreeCodec(t *testing.T) {
	tree := object.NewTree()
	tree.Entries["a.go"] = object.Entry{Name: "a.go", Kind: object.KindBlob, ID: object.BlobID([]byte("package a"))}
	tree.Entries["subdir"] = object.Entry{Name: "subdir", Kind: object.KindTree, ID: object.BlobID([]byte("tree"))}
	payload := EncodeTree(tree)
	dec, err := DecodeTree(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(dec.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(dec.Entries))
	}
	if dec.Entries["a.go"].ID != tree.Entries["a.go"].ID {
		t.Fatal("a.go id mismatch")
	}
	if dec.Entries["subdir"].Kind != object.KindTree {
		t.Fatal("subdir kind mismatch")
	}
}

func TestTreeCodecTruncated(t *testing.T) {
	// Claims 3 entries but payload is short.
	payload := []byte{0x03, 0x01} // count=3, then name len=1
	if _, err := DecodeTree(payload); err == nil {
		t.Fatal("expected error for truncated tree")
	}
	// A valid header with an entry whose id block is short.
	// count=1, name="a" (1), kind=blob(0), then 0 id bytes.
	short := append([]byte{0x01}, 0x01, 'a', 0x00)
	if _, err := DecodeTree(short); err == nil {
		t.Fatal("expected error for truncated tree id")
	}
}

func TestUvarintRoundTrip(t *testing.T) {
	vals := []uint64{0, 1, 127, 128, 300, 65535, 1 << 32, 1<<63 + 5}
	for _, v := range vals {
		enc := appendUvarint(nil, v)
		got, adv := readUvarint(enc)
		if got != v || adv != len(enc) {
			t.Fatalf("v=%d got=%d adv=%d len=%d", v, got, adv, len(enc))
		}
	}
}
