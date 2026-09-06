// Package transfer implements the wire-format for moving repository state
// between two stores. A Bundle carries everything needed to reproduce part of
// a repository: content-addressed objects (global, deduplicated), per-repo
// snapshots, changes, and refs. It is used both for export/import to a file and
// for the EasyVCS smart protocol between the CLI and the server.
package transfer

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"

	"easyvcs/internal/object"
	"easyvcs/internal/store"
)

// Version is the bundle format version.
const Version = 1

// bucketMagic identifies a binary-encoded bundle (used for network transport to
// avoid base64 inflation of object contents).
var bucketMagic = []byte("EVCSBUN\x00")

// Bundle is a serializable slice of a repository.
type Bundle struct {
	Version   int               `json:"version"`
	Repo      store.RepoRef     `json:"repo"`
	Changes   []*store.Revision `json:"changes"`
	Snapshots []*store.Snapshot `json:"snapshots"`
	Refs      []*store.Ref      `json:"refs"`
	Objects   []ObjectRecord    `json:"objects"`
}

// ObjectRecord is an encoded content-addressed object.
type ObjectRecord struct {
	Kind    object.Kind `json:"kind"`
	Content []byte      `json:"content"`
}

// binHeader describes the JSON-encoded metadata of a binary bundle, followed by
// the concatenated raw object payloads.
type binHeader struct {
	Version   int               `json:"version"`
	Repo      store.RepoRef     `json:"repo"`
	Changes   []*store.Revision `json:"changes"`
	Snapshots []*store.Snapshot `json:"snapshots"`
	Refs      []*store.Ref      `json:"refs"`
	Objects   []binObject       `json:"objects"`
}

// binObject records an object descriptor (kind + payload length) in a binary
// bundle; the payload bytes follow the header contiguously.
type binObject struct {
	Kind int `json:"kind"`
	Len  int `json:"len"`
}

// MarshalBinary encodes a bundle into a self-describing binary frame: a JSON
// metadata header (repo, changes, snapshots, refs, object descriptors) followed
// by the raw object contents. This avoids the ~33% base64 overhead of putting
// object content in JSON. It is optionally gzip-compressed by the caller.
func (b *Bundle) MarshalBinary() ([]byte, error) {
	header := binHeader{
		Version: b.Version, Repo: b.Repo,
		Changes: b.Changes, Snapshots: b.Snapshots, Refs: b.Refs,
	}
	var body bytes.Buffer
	for _, ob := range b.Objects {
		header.Objects = append(header.Objects, binObject{Kind: int(ob.Kind), Len: len(ob.Content)})
		body.Write(ob.Content)
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	buf.Write(bucketMagic)
	var lenBuf [8]byte
	binary.LittleEndian.PutUint32(lenBuf[0:4], uint32(len(headerJSON)))
	binary.LittleEndian.PutUint32(lenBuf[4:8], uint32(body.Len()))
	buf.Write(lenBuf[:])
	buf.Write(headerJSON)
	buf.Write(body.Bytes())
	return buf.Bytes(), nil
}

// UnmarshalBinary decodes a bundle produced by MarshalBinary. If data is a
// gzip stream (starts with the gzip magic 0x1f 0x8b), it is decompressed first.
func UnmarshalBinary(data []byte) (*Bundle, error) {
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		gr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer gr.Close()
		raw, err := io.ReadAll(gr)
		if err != nil {
			return nil, err
		}
		data = raw
	}
	if !bytes.HasPrefix(data, bucketMagic) {
		return nil, fmt.Errorf("not a binary bundle")
	}
	data = data[len(bucketMagic):]
	if len(data) < 8 {
		return nil, fmt.Errorf("truncated bundle header")
	}
	headerLen := int(binary.LittleEndian.Uint32(data[0:4]))
	bodyLen := int(binary.LittleEndian.Uint32(data[4:8]))
	data = data[8:]
	if len(data) < headerLen+bodyLen {
		return nil, fmt.Errorf("truncated bundle body")
	}
	headerJSON := data[:headerLen]
	body := data[headerLen : headerLen+bodyLen]
	var header binHeader
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, err
	}
	b := &Bundle{
		Version: header.Version, Repo: header.Repo,
		Changes: header.Changes, Snapshots: header.Snapshots, Refs: header.Refs,
	}
	offset := 0
	for _, od := range header.Objects {
		if offset+od.Len > len(body) {
			return nil, fmt.Errorf("truncated object payload")
		}
		b.Objects = append(b.Objects, ObjectRecord{
			Kind:    object.Kind(od.Kind),
			Content: body[offset : offset+od.Len],
		})
		offset += od.Len
	}
	return b, nil
}

// CompressBundle encodes a bundle to its binary frame and gzip-compresses it.
func CompressBundle(b *Bundle) ([]byte, error) {
	raw, err := b.MarshalBinary()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(raw); err != nil {
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Collector collects reachable objects from a repository.
type Collector interface {
	Repo() *store.Repo
}

// Collect produces a Bundle containing every change, snapshot, ref, and any
// object referenced by those snapshots. When `have` is non-nil, objects that
// already exist in the destination (per hasObject) are skipped; this yields a
// delta rather than the full bundle.
//
// hasObject, if non-nil, reports whether a given content id already exists at
// the destination. When nil, all reachable objects are included.
func Collect(repo *store.Repo, have []string, hasObject func(id object.ID) bool) (*Bundle, error) {
	changes, err := repo.ListRevisions()
	if err != nil {
		return nil, err
	}
	refs, err := repo.ListRefs()
	if err != nil {
		return nil, err
	}

	// Track which change ids we've already collected (dedupe on rewrite).
	haveSet := map[string]bool{}
	for _, c := range have {
		haveSet[c] = true
	}

	b := &Bundle{Version: Version, Repo: repo.RepoRef()}
	// Skip snapshots/changes that the destination already has (by change id).
	for _, ch := range changes {
		if haveSet[ch.ID] {
			continue
		}
		snap, err := repo.GetSnapshot(ch.Hash)
		if err != nil {
			continue
		}
		b.Changes = append(b.Changes, ch)
		b.Snapshots = append(b.Snapshots, snap)
		if err := collectObjects(repo, snap.TreeID, b, hasObject); err != nil {
			return nil, err
		}
	}
	b.Refs = refs
	return b, nil
}

// EnumerateObjectIDs returns every content-addressed object id in a repository.
// It is used by a client to advertise "have" objects so a peer can compute a
// delta (send only objects it does not already have).
func EnumerateObjectIDs(repo *store.Repo) ([]object.ID, error) {
	return repo.ObjectIDs()
}

// CollectObjectsWhere collects only the objects reachable from a snapshot tree
// that are NOT already present (per hasObject), returning them as records. This
// is a low-level helper used by server-side fetch to skip known objects.
func CollectObjectsWhere(repo *store.Repo, root object.ID, hasObject func(id object.ID) bool) ([]ObjectRecord, error) {
	b := &Bundle{}
	if err := collectObjects(repo, root, b, hasObject); err != nil {
		return nil, err
	}
	return b.Objects, nil
}

// CollectAll returns a full bundle (no delta skipping).
func CollectAll(repo *store.Repo) (*Bundle, error) {
	return Collect(repo, nil, nil)
}

func collectObjects(repo *store.Repo, root object.ID, b *Bundle, hasObject func(id object.ID) bool) error {
	seen := map[string]bool{}
	var walk func(id object.ID) error
	walk = func(id object.ID) error {
		if seen[id.String()] {
			return nil
		}
		// If the destination already has this object, skip it (delta).
		if hasObject != nil && hasObject(id) {
			seen[id.String()] = true
			return nil
		}
		o, err := repo.ReadObject(id)
		if err != nil {
			return err
		}
		payload, err := object.EncodeObject(o)
		if err != nil {
			return err
		}
		b.Objects = append(b.Objects, ObjectRecord{Kind: o.Kind, Content: payload})
		seen[id.String()] = true
		if o.Kind == object.KindTree && o.Tree != nil {
			for _, e := range o.Tree.SortedEntries() {
				if err := walk(e.ID); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(root)
}

// Apply writes the contents of a bundle into a repository. Objects are written
// first (content-addressed, idempotent), then snapshots, changes, and refs.
// Objects are inserted in a single transaction for efficiency. It returns the
// number of changes written.
func Apply(repo *store.Repo, b *Bundle) (int, error) {
	// Decode all objects first so we can batch them; content-addressed writes
	// are idempotent (INSERT ... ON CONFLICT DO NOTHING).
	objs := make([]*object.Object, 0, len(b.Objects))
	for _, ob := range b.Objects {
		obj, err := decodeObjectRecord(ob)
		if err != nil {
			return 0, err
		}
		objs = append(objs, obj)
	}
	if err := repo.WriteObjectsBatch(objs); err != nil {
		return 0, err
	}
	for _, snap := range b.Snapshots {
		if err := repo.PutSnapshot(snap); err != nil {
			return 0, err
		}
	}
	for _, ch := range b.Changes {
		if err := repo.PutRevision(ch); err != nil {
			return 0, err
		}
	}
	for _, r := range b.Refs {
		if err := repo.PutRef(r); err != nil {
			return 0, err
		}
	}
	return len(b.Changes), nil
}

func decodeObjectRecord(ob ObjectRecord) (*object.Object, error) {
	switch ob.Kind {
	case object.KindBlob:
		return &object.Object{Kind: object.KindBlob, Blob: ob.Content}, nil
	case object.KindTree:
		o, err := object.DecodeObject(object.BlobID(ob.Content), ob.Content)
		if err != nil {
			return nil, err
		}
		if o.Kind != object.KindTree || o.Tree == nil {
			return nil, fmt.Errorf("expected tree payload, got %v", o.Kind)
		}
		return o, nil
	case object.KindConflict:
		o, err := object.DecodeObject(object.BlobID(ob.Content), ob.Content)
		if err != nil {
			return nil, err
		}
		if o.Kind != object.KindConflict || o.Conflict == nil {
			return nil, fmt.Errorf("expected conflict payload, got %v", o.Kind)
		}
		return o, nil
	default:
		return nil, fmt.Errorf("unsupported object kind %v", ob.Kind)
	}
}
