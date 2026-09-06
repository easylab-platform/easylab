package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	"easyvcs/internal/object"
	"easyvcs/internal/store"
)

// bundle holds a serialized repository for export/import. It carries the repo
// ref plus all of its changes, snapshots, refs, and the referenced objects.
type bundle struct {
	Version   int               `json:"version"`
	Repo      store.RepoRef     `json:"repo"`
	Changes   []*store.Revision `json:"changes"`
	Snapshots []*store.Snapshot `json:"snapshots"`
	Refs      []*store.Ref      `json:"refs"`
	Objects   []bundleObject    `json:"objects"`
}

type bundleObject struct {
	Kind    object.Kind `json:"kind"`
	Content []byte      `json:"content"` // encoded payload
}

func cmdExport(c *ctx) {
	var out string
	var positional []string
	for i := 0; i < len(os.Args[2:]); i++ {
		switch os.Args[2+i] {
		case "-o", "--o", "-out":
			if i+1 < len(os.Args)-2 {
				out = os.Args[2+i+1]
				i++
			}
		default:
			positional = append(positional, os.Args[2+i])
		}
	}
	if len(positional) < 1 || out == "" {
		fmt.Fprintln(os.Stderr, "usage: export <ns/name> -o FILE")
		os.Exit(1)
	}
	arg := positional[0]
	ns, name, err := splitRepo(arg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "export:", err)
		os.Exit(1)
	}
	repo, err := c.cs.OpenRepo(store.RepoRef{Namespace: ns, Name: name})
	if err != nil {
		fmt.Fprintln(os.Stderr, "export:", err)
		os.Exit(1)
	}

	changes, err := repo.ListRevisions()
	if err != nil {
		fmt.Fprintln(os.Stderr, "export:", err)
		os.Exit(1)
	}
	refs, err := repo.ListRefs()
	if err != nil {
		fmt.Fprintln(os.Stderr, "export:", err)
		os.Exit(1)
	}

	var snaps []*store.Snapshot
	objSet := map[string]bool{}
	var objs []bundleObject

	var collect func(id object.ID) error
	collect = func(id object.ID) error {
		if objSet[id.String()] {
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
		objSet[id.String()] = true
		objs = append(objs, bundleObject{Kind: o.Kind, Content: payload})

		// Recurse into tree children so nested blobs are included.
		if o.Kind == object.KindTree && o.Tree != nil {
			for _, e := range o.Tree.SortedEntries() {
				if err := collect(e.ID); err != nil {
					return err
				}
			}
		}
		return nil
	}

	for _, ch := range changes {
		snap, err := repo.GetSnapshot(ch.Hash)
		if err != nil {
			continue
		}
		snaps = append(snaps, snap)
		if err := collect(snap.TreeID); err != nil {
			fmt.Fprintln(os.Stderr, "export:", err)
			os.Exit(1)
		}
	}

	b := bundle{Version: 1, Repo: repo.RepoRef(), Changes: changes, Snapshots: snaps, Refs: refs, Objects: objs}
	f, err := os.Create(out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "export:", err)
		os.Exit(1)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	if err := enc.Encode(b); err != nil {
		fmt.Fprintln(os.Stderr, "export:", err)
		os.Exit(1)
	}
	if err := w.Flush(); err != nil {
		fmt.Fprintln(os.Stderr, "export:", err)
		os.Exit(1)
	}
	fmt.Printf("exported %s -> %s (%d changes, %d objects)\n", repo, out, len(changes), len(objs))
}

func cmdImport(c *ctx) {
	var file string
	var nsOverride string
	for i := 0; i < len(os.Args[2:]); i++ {
		switch os.Args[2+i] {
		case "-n", "--n", "-namespace":
			if i+1 < len(os.Args)-2 {
				nsOverride = os.Args[2+i+1]
				i++
			}
		default:
			file = os.Args[2+i]
		}
	}
	if file == "" {
		fmt.Fprintln(os.Stderr, "usage: import FILE [-n namespace]")
		os.Exit(1)
	}
	f, err := os.Open(file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "import:", err)
		os.Exit(1)
	}
	defer f.Close()
	var b bundle
	if err := json.NewDecoder(bufio.NewReader(f)).Decode(&b); err != nil {
		fmt.Fprintln(os.Stderr, "import:", err)
		os.Exit(1)
	}

	repoRef := b.Repo
	if nsOverride != "" {
		repoRef.Namespace = nsOverride
	}

	// Create (or reuse) the repository.
	var repo *store.Repo
	exists, err := c.cs.RepoExists(repoRef)
	if err != nil {
		fmt.Fprintln(os.Stderr, "import:", err)
		os.Exit(1)
	}
	if exists {
		repo, err = c.cs.OpenRepo(repoRef)
		if err != nil {
			fmt.Fprintln(os.Stderr, "import:", err)
			os.Exit(1)
		}
	} else {
		repo, err = c.cs.Create(repoRef)
		if err != nil {
			fmt.Fprintln(os.Stderr, "import:", err)
			os.Exit(1)
		}
	}

	// Write objects first (content-addressed, global dedup).
	for _, ob := range b.Objects {
		o, err := object.DecodeObject(object.BlobID(ob.Content), ob.Content)
		if err == nil {
			// DecodeObject needs the real id; re-encode to compute it.
			_ = o
		}
		obj, err := decodeBundleObject(ob)
		if err != nil {
			fmt.Fprintln(os.Stderr, "import:", err)
			os.Exit(1)
		}
		if err := repo.WriteObject(obj); err != nil {
			fmt.Fprintln(os.Stderr, "import:", err)
			os.Exit(1)
		}
	}
	for _, snap := range b.Snapshots {
		if err := repo.PutSnapshot(snap); err != nil {
			fmt.Fprintln(os.Stderr, "import:", err)
			os.Exit(1)
		}
	}
	for _, ch := range b.Changes {
		if err := repo.PutRevision(ch); err != nil {
			fmt.Fprintln(os.Stderr, "import:", err)
			os.Exit(1)
		}
	}
	for _, r := range b.Refs {
		if err := repo.PutRef(r); err != nil {
			fmt.Fprintln(os.Stderr, "import:", err)
			os.Exit(1)
		}
	}
	fmt.Printf("imported %s (%d changes)\n", repoRef, len(b.Changes))
}

func decodeBundleObject(ob bundleObject) (*object.Object, error) {
	// Re-encode to compute the true content id from the payload.
	o, err := object.DecodeObject(object.BlobID([]byte("x")), ob.Content)
	if err != nil {
		// Fallback: treat as blob.
		return &object.Object{Kind: object.KindBlob, Blob: ob.Content}, nil
	}
	_ = o
	switch ob.Kind {
	case object.KindBlob:
		return &object.Object{Kind: object.KindBlob, Blob: ob.Content}, nil
	case object.KindTree:
		t, err := decodeTreePayload(ob.Content)
		if err != nil {
			return nil, err
		}
		return &object.Object{Kind: object.KindTree, Tree: t}, nil
	default:
		return &object.Object{Kind: ob.Kind}, nil
	}
}

func decodeTreePayload(payload []byte) (*object.Tree, error) {
	// Use the object package's decoder by reconstructing with a fake id.
	o, err := object.DecodeObject(object.BlobID(payload), payload)
	if err != nil {
		return nil, err
	}
	if o.Kind != object.KindTree || o.Tree == nil {
		return nil, fmt.Errorf("expected tree, got %v", o.Kind)
	}
	return o.Tree, nil
}
