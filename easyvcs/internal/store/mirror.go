package store

import (
	"database/sql"
)

// PushMirror is a named external git destination that receives a force push of
// a single branch's tree. A normal (read-write) repository may have several.
type PushMirror struct {
	ID        int64
	RepoID    int64
	Name      string
	URL       string
	Branch    string // branch name to push (default "main")
	Token     string
	LastRev   string // last revision id pushed (dirty detection)
	LastError string
}

// PutPushMirror inserts or replaces a push-mirror for this repo.
func (r *Repo) PutPushMirror(m *PushMirror) error {
	_, err := r.cs.d.exec(
		`INSERT INTO push_mirrors(repo_id, name, url, branch, token, last_rev, last_error)
		 VALUES(?,?,?,?,?,?,?)
		 ON CONFLICT(repo_id, name) DO UPDATE SET
		   url=excluded.url, branch=excluded.branch, token=excluded.token,
		   last_rev=excluded.last_rev, last_error=excluded.last_error`,
		r.repoID, m.Name, m.URL, m.Branch, m.Token, m.LastRev, m.LastError,
	)
	return err
}

// GetPushMirror returns one push-mirror by name.
func (r *Repo) GetPushMirror(name string) (*PushMirror, error) {
	var id, repoID int64
	var url, branch, token, lastRev, lastErr string
	err := r.cs.d.queryRow(
		`SELECT id, repo_id, url, branch, token, last_rev, last_error
		 FROM push_mirrors WHERE repo_id=? AND name=?`,
		r.repoID, name,
	).Scan(&id, &repoID, &url, &branch, &token, &lastRev, &lastErr)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &PushMirror{ID: id, RepoID: repoID, Name: name, URL: url,
		Branch: branch, Token: token, LastRev: lastRev, LastError: lastErr}, nil
}

// ListPushMirrors lists all push-mirrors for this repo (tokens omitted).
func (r *Repo) ListPushMirrors() ([]*PushMirror, error) {
	rows, err := r.cs.d.query(
		`SELECT id, name, url, branch, last_rev, last_error FROM push_mirrors WHERE repo_id=? ORDER BY name`,
		r.repoID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PushMirror
	for rows.Next() {
		var m PushMirror
		if err := rows.Scan(&m.ID, &m.Name, &m.URL, &m.Branch, &m.LastRev, &m.LastError); err != nil {
			return nil, err
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

// DeletePushMirror removes a push-mirror by name.
func (r *Repo) DeletePushMirror(name string) error {
	res, err := r.cs.d.exec("DELETE FROM push_mirrors WHERE repo_id=? AND name=?", r.repoID, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdatePushMirrorSync records the last pushed revision / error for a push-mirror.
func (r *Repo) UpdatePushMirrorSync(name, lastRev, lastErr string) error {
	_, err := r.cs.d.exec(
		"UPDATE push_mirrors SET last_rev=?, last_error=? WHERE repo_id=? AND name=?",
		lastRev, lastErr, r.repoID, name,
	)
	return err
}

// CreateMirror creates a read-only mirror repository and returns its handle.
func (s *CentralStore) CreateMirror(r RepoRef, meta RepoMeta) (*Repo, error) {
	if meta.Kind == "" {
		meta.Kind = "mirror"
	}
	repo, err := s.Create(r)
	if err != nil {
		return nil, err
	}
	if meta.MirrorInterval <= 0 {
		meta.MirrorInterval = 300
	}
	if meta.MirrorBranch == "" {
		meta.MirrorBranch = meta.DefaultBranch
		if meta.MirrorBranch == "" {
			meta.MirrorBranch = "main"
		}
	}
	if err := repo.UpdateMirrorMeta(meta); err != nil {
		return nil, err
	}
	return repo, nil
}

// AllPushMirrors lists every push-mirror across all repositories (scheduler).
func (s *CentralStore) AllPushMirrors() ([]*PushMirror, error) {
	rows, err := s.d.query(
		`SELECT pm.id, pm.repo_id, pm.name, pm.url, pm.branch, pm.token, pm.last_rev, pm.last_error,
		        r.namespace, r.name
		 FROM push_mirrors pm JOIN repositories r ON r.id=pm.repo_id ORDER BY pm.repo_id, pm.name`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PushMirror
	for rows.Next() {
		var m PushMirror
		var ns, name string
		if err := rows.Scan(&m.ID, &m.RepoID, &m.Name, &m.URL, &m.Branch, &m.Token, &m.LastRev, &m.LastError, &ns, &name); err != nil {
			return nil, err
		}
		m.Name = ns + "/" + name + "|" + m.Name
		out = append(out, &m)
	}
	return out, rows.Err()
}

// AllMirrors lists every read-only mirror repository (scheduler).
func (s *CentralStore) AllMirrors() ([]*Repo, RepoMetaMap, error) {
	rows, err := s.d.query(
		`SELECT id, namespace, name, kind, mirror_url, mirror_branch,
		        mirror_interval, mirror_last_rev, mirror_last_sync,
		        mirror_last_error, mirror_token
		 FROM repositories WHERE kind='mirror' ORDER BY namespace, name`,
	)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var repos []*Repo
	meta := RepoMetaMap{}
	for rows.Next() {
		var id int64
		var ns, name, kind, url, branch, lastRev, lastErr, token string
		var interval, lastSync int64
		if err := rows.Scan(&id, &ns, &name, &kind, &url, &branch, &interval, &lastRev, &lastSync, &lastErr, &token); err != nil {
			return nil, nil, err
		}
		repos = append(repos, &Repo{cs: s, repoID: id, Namespace: ns, Name: name})
		meta[nameOf(ns, name)] = RepoMeta{
			Kind: kind, MirrorURL: url, MirrorBranch: branch, MirrorInterval: int(interval),
			MirrorLastRev: lastRev, MirrorLastSync: lastSync, MirrorLastErr: lastErr, MirrorToken: token,
		}
	}
	return repos, meta, rows.Err()
}

// RepoMetaMap keys a RepoMeta by "namespace/name".
type RepoMetaMap map[string]RepoMeta

func nameOf(ns, name string) string { return ns + "/" + name }

// TouchMirrorSync updates a mirror's last sync time/rev/error.
func (r *Repo) TouchMirrorSync(lastRev string, syncEpoch int64, lastErr string) error {
	_, err := r.cs.d.exec(
		`UPDATE repositories SET mirror_last_rev=?, mirror_last_sync=?, mirror_last_error=? WHERE id=?`,
		lastRev, syncEpoch, lastErr, r.repoID,
	)
	return err
}
