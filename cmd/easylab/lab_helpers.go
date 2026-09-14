package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/revision"
	"github.com/easylab-platform/easyvcs/store"
)

// errUnauthorizedWrite is the sentinel for an unauthenticated write.
var errUnauthorizedWrite = errors.New("unauthorized: a valid token is required to write")

// releaseRepository maps a repo to its generic-artifact release key. A Lab
// release maps to an artifact in the "generic" format keyed by "ns:repo" (a
// colon, so the generic URL path stays flat).
func releaseRepository(repo *store.Repo) string { return repo.Namespace + ":" + repo.Name }

// labReleaseMeta round-trips name/description/revision/draft/prerelease through
// the artifact's proprietary bytes.
type labReleaseMeta struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	RevisionID  string    `json:"revision_id"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	Created     time.Time `json:"created"`
}

// treeOf resolves a ref/name to a tree object id.
func treeOf(ws *revision.Workspace, repo *store.Repo, ref string) (object.ID, error) {
	return treeOfRef(ws, repo, ref)
}

// resolveSnapshotOf resolves a ref/name to its snapshot.
func resolveSnapshotOf(ws *revision.Workspace, repo *store.Repo, ref string) (*store.Snapshot, error) {
	id, err := resolveRefAny(ws, repo, ref)
	if err != nil {
		return nil, err
	}
	return repo.GetSnapshot(id)
}

// resolveRefAny resolves a rev expression (branch / tag / sha / revision id /
// "@") to a snapshot hash (object id). It mirrors the resolve semantics the
// removed REST handler used.

func resolveRefAny(ws *revision.Workspace, repo *store.Repo, ref string) (object.ID, error) {
	if ref == "" || ref == "@" {
		revs, err := ws.Log()
		if err != nil || len(revs) == 0 {
			return object.ID{}, fmt.Errorf("no revisions")
		}
		return revs[0].Hash, nil
	}
	if ref == "@-" {
		revs, err := ws.Log()
		if err != nil || len(revs) == 0 {
			return object.ID{}, fmt.Errorf("no revisions")
		}
		snap, err := repo.GetSnapshot(revs[0].Hash)
		if err != nil {
			return object.ID{}, err
		}
		if len(snap.Parents) == 0 {
			return object.ID{}, fmt.Errorf("no parent")
		}
		return snap.Parents[0], nil
	}
	// Try a ref (branch/tag).
	if rf, err := ws.GetRef(ref); err == nil && rf != nil {
		return resolveRefAny(ws, repo, rf.Target)
	}
	// Try a snapshot hash directly.
	if id, err := object.HexToID(ref); err == nil {
		if _, err := repo.GetSnapshot(id); err == nil {
			return id, nil
		}
	}
	// Revisions: exact id or prefix.
	revs, err := ws.Log()
	if err != nil {
		return object.ID{}, err
	}
	for _, rv := range revs {
		if rv.ID == ref {
			return rv.Hash, nil
		}
	}
	for _, rv := range revs {
		if strings.HasPrefix(rv.ID, ref) {
			return rv.Hash, nil
		}
	}
	return object.ID{}, fmt.Errorf("unknown ref %q", ref)
}

// resolveRevID resolves a rev expression to its stable revision id.

func resolveRevID(ws *revision.Workspace, repo *store.Repo, ref string) (string, error) {
	hash, err := resolveRefAny(ws, repo, ref)
	if err != nil {
		return "", err
	}
	revs, err := ws.Log()
	if err != nil {
		return "", err
	}
	for _, rv := range revs {
		if rv.Hash == hash {
			return rv.ID, nil
		}
	}
	// Ref pointed at a raw snapshot hash not owned by a listed revision (rare);
	// best effort: return the hash as a revision id lookup will fail.
	return hash.String(), nil
}
