package main

import (
	"errors"
	"fmt"
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
		ref = ""
	}
	// A branch/tag ref name resolves to its stored target (a snapshot hash).
	if refs, err := ws.ListRefs(); err == nil {
		for _, r := range refs {
			if r.Name != ref {
				continue
			}
			if id, herr := object.HexToID(r.Target); herr == nil {
				return id, nil
			}
			// The ref target may be a revision id; resolve it to its hash.
			if rev, rerr := repo.GetRevision(r.Target); rerr == nil {
				return rev.Hash, nil
			}
		}
	}
	// A snapshot hash directly.
	if id, err := object.HexToID(ref); err == nil {
		if _, serr := repo.GetSnapshot(id); serr == nil {
			return id, nil
		}
	}
	// A revision id -> its snapshot hash.
	if rev, err := repo.GetRevision(ref); err == nil {
		return rev.Hash, nil
	}
	// Repo HEAD fallback.
	if ref == "" {
		revs, err := repo.ListRevisions()
		if err == nil && len(revs) > 0 {
			return revs[len(revs)-1].Hash, nil
		}
	}
	return object.ID{}, fmt.Errorf("cannot resolve %q", ref)
}

// resolveRevID resolves a rev expression to its stable revision id.
func resolveRevID(ws *revision.Workspace, repo *store.Repo, ref string) (string, error) {
	if ref == "" || ref == "@" {
		// Latest revision.
		revs, err := repo.ListRevisions()
		if err != nil {
			return "", err
		}
		if len(revs) == 0 {
			return "", fmt.Errorf("no revisions")
		}
		return revs[len(revs)-1].ID, nil
	}
	if refs, err := ws.ListRefs(); err == nil {
		for _, r := range refs {
			if r.Name != ref {
				continue
			}
			// Target may be a revision id or a snapshot hash.
			if _, rerr := repo.GetRevision(r.Target); rerr == nil {
				return r.Target, nil
			}
			if id, herr := object.HexToID(r.Target); herr == nil {
				if revs, rerr := repo.ListRevisions(); rerr == nil {
					for _, rev := range revs {
						if rev.Hash == id {
							return rev.ID, nil
						}
					}
				}
			}
		}
	}
	if _, err := repo.GetRevision(ref); err == nil {
		return ref, nil
	}
	if id, err := object.HexToID(ref); err == nil {
		if revs, rerr := repo.ListRevisions(); rerr == nil {
			for _, rev := range revs {
				if rev.Hash == id {
					return rev.ID, nil
				}
			}
		}
	}
	return "", fmt.Errorf("cannot resolve revision %q", ref)
}
