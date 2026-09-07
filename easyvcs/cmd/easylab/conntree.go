package main

import (
	"fmt"

	"easyvcs/internal/object"
	"easyvcs/internal/revision"
	"easyvcs/internal/store"
)

// treeOfRef resolves a ref ("" = latest, "@", branch, tag, revision id) to the
// tree id of its snapshot. Mirrors lab.go's treeOf for the Connect surface.
func treeOfRef(ws *revision.Workspace, repo *store.Repo, ref string) (object.ID, error) {
	if ref == "" {
		revs, err := ws.Log()
		if err != nil || len(revs) == 0 {
			return object.ID{}, fmt.Errorf("no revisions")
		}
		snap, err := repo.GetSnapshot(revs[0].Hash)
		if err != nil {
			return object.ID{}, err
		}
		return snap.TreeID, nil
	}
	id, err := resolveRefAny(ws, repo, ref)
	if err != nil {
		return object.ID{}, err
	}
	snap, err := repo.GetSnapshot(id)
	if err != nil {
		return object.ID{}, err
	}
	return snap.TreeID, nil
}
