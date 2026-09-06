package revision

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"easyvcs/internal/ignore"
	"easyvcs/internal/object"
	"easyvcs/internal/store"
)

// pathKey sorts paths deterministically.
func pathKey(s string) string { return s }

// RewriteHistoryWithIgnores rewrites all snapshots that are ancestors of
// changeID so that any path now matched by the matcher is removed from every
// affected snapshot's tree. Each rewritten snapshot keeps its change id and
// metadata; only the tree (and thus the snapshot id) changes. Descendants are
// automatically rebased because they reference parent snapshot ids.
//
// It returns the number of snapshots rewritten.
func (w *Workspace) RewriteHistoryWithIgnores(root string, m *ignore.Matcher, changeID string) (int, error) {
	// Collect all ancestor revision ids (including changeID itself) via a BFS
	// over parent snapshot ownership.
	allRevisions, err := w.store.ListRevisions()
	if err != nil {
		return 0, err
	}
	byID := map[string]*store.Revision{}
	for _, c := range allRevisions {
		byID[c.ID] = c
	}
	snapOwner := map[string]string{}
	for _, c := range allRevisions {
		snapOwner[c.Hash.String()] = c.ID
	}

	visited := map[string]bool{}
	var order []string // breadth-first ancestors; not strictly topological
	queue := []string{changeID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if visited[id] {
			continue
		}
		visited[id] = true
		order = append(order, id)
		ch := byID[id]
		if ch == nil {
			continue
		}
		snap, err := w.store.GetSnapshot(ch.Hash)
		if err != nil {
			continue
		}
		for _, p := range snap.Parents {
			if owner, ok := snapOwner[p.String()]; ok && !visited[owner] {
				queue = append(queue, owner)
			}
		}
	}

	// A: short-circuit when no ignore pattern could reach the repo root at all.
	if !m.HasAnyMatchUnder("") {
		return 0, nil
	}

	rewritten := 0
	memo := map[string]object.ID{} // old snapshot hash -> new snapshot hash
	var visit func(id string) error
	visit = func(id string) error {
		if rewrittenByVisit[id] {
			return nil
		}
		ch := byID[id]
		if ch == nil {
			return nil
		}
		cur, err := w.store.GetSnapshot(ch.Hash)
		if err != nil {
			return err
		}
		// Recurse into parents first.
		var newParents []object.ID
		for _, p := range cur.Parents {
			if owner, ok := snapOwner[p.String()]; ok {
				if err := visit(owner); err != nil {
					return err
				}
			}
			if np, ok := memo[p.String()]; ok {
				newParents = append(newParents, np)
			} else {
				newParents = append(newParents, p)
			}
		}
		// Filter this snapshot's tree.
		newTreeID, err := w.DropIgnoredPathsFromTree(root, m, cur.TreeID)
		if err != nil {
			return err
		}
		newSnap := &store.Snapshot{
			RevisionID:  cur.RevisionID,
			Parents:     newParents,
			TreeID:      newTreeID,
			Description: cur.Description,
			Author:      cur.Author,
			CommitTime:  cur.CommitTime,
		}
		newID := snapshotID(newSnap)
		if newID != cur.RevisionHash {
			rewritten++
			newSnap.RevisionHash = newID
			if err := w.store.PutSnapshot(newSnap); err != nil {
				return err
			}
			if err := w.store.UpdateRevisionHash(id, newID); err != nil {
				return err
			}
			memo[cur.RevisionHash.String()] = newID
		} else {
			memo[cur.RevisionHash.String()] = cur.RevisionHash
		}
		rewrittenByVisit[id] = true
		return nil
	}
	rewrittenByVisit = map[string]bool{}
	if err := visit(changeID); err != nil {
		return 0, err
	}
	return rewritten, nil
}

var rewrittenByVisit map[string]bool

// IgnoreHash computes a stable hash over the current ignore files at root, for
// detecting when ignore rules change between commits. Files are sorted so the
// result is order-independent.
func IgnoreHash(root string) (string, error) {
	h := sha256.New()
	files, err := collectIgnoreFiles(root)
	if err != nil {
		return "", err
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\n", filepath.ToSlash(f))
		h.Write(data)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func collectIgnoreFiles(root string) ([]string, error) {
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info == nil || info.IsDir() {
			return nil
		}
		name := filepath.Base(path)
		if name == ".gitignore" || name == ".vcsignore" {
			out = append(out, path)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

var _ = pathKey
var _ = store.ErrNotFound
