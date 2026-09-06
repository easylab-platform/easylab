package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"easyvcs/internal/encoding"
	"easyvcs/internal/object"
)

// sqlDialect abstracts the placeholder differences between SQLite and Postgres.
type sqlDialect struct {
	db     *sql.DB
	kind   string
	rebind func(string) string
}

func (d *sqlDialect) rebindQ(q string) string {
	if d.rebind == nil {
		return q
	}
	return d.rebind(q)
}

func (d *sqlDialect) exec(q string, args ...any) (sql.Result, error) {
	return d.db.Exec(d.rebindQ(q), args...)
}

func (d *sqlDialect) query(q string, args ...any) (*sql.Rows, error) {
	return d.db.Query(d.rebindQ(q), args...)
}

func (d *sqlDialect) queryRow(q string, args ...any) *sql.Row {
	return d.db.QueryRow(d.rebindQ(q), args...)
}

func (d *sqlDialect) execTx(tx *sql.Tx, q string, args ...any) (sql.Result, error) {
	return tx.Exec(d.rebindQ(q), args...)
}

func (d *sqlDialect) queryTx(tx *sql.Tx, q string, args ...any) (*sql.Rows, error) {
	return tx.Query(d.rebindQ(q), args...)
}

// CentralStore is the single-database store. It holds many repositories, each
// identified by (namespace, name). Objects are global (content-addressed and
// deduplicated); metadata tables are scoped by a repo id.
type CentralStore struct {
	d    *sqlDialect
	root string
}

// HomeDir returns the EasyVCS home directory (default ~/.easyvcs, overridable
// via EASYVCS_HOME).
func HomeDir() string {
	if h := os.Getenv("EASYVCS_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".easyvcs"
	}
	return filepath.Join(home, ".easyvcs")
}

// DBPath returns the path to the central database file.
func DBPath() string { return filepath.Join(HomeDir(), DefaultDBFile) }

// SetWAL enables SQLite WAL mode for safe concurrent access across processes
// (many workspaces sharing one DB). No-op for Postgres.
func (s *CentralStore) SetWAL() error {
	if s.d.kind != KindSQLite {
		return nil
	}
	_, err := s.d.exec("PRAGMA journal_mode=WAL")
	return err
}

// Init creates the schema. Idempotent.
func (s *CentralStore) Init() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS repositories (
			id INTEGER PRIMARY KEY,
			namespace TEXT NOT NULL,
			name TEXT NOT NULL,
			created INTEGER NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			visibility TEXT NOT NULL DEFAULT 'public',
			default_branch TEXT NOT NULL DEFAULT 'main',
			kind TEXT NOT NULL DEFAULT 'normal',
			mirror_url TEXT NOT NULL DEFAULT '',
			mirror_branch TEXT NOT NULL DEFAULT 'main',
			mirror_interval INTEGER NOT NULL DEFAULT 300,
			mirror_last_rev TEXT NOT NULL DEFAULT '',
			mirror_last_sync INTEGER NOT NULL DEFAULT 0,
			mirror_last_error TEXT NOT NULL DEFAULT '',
			mirror_token TEXT NOT NULL DEFAULT '',
			UNIQUE(namespace, name)
		)`,
		`CREATE TABLE IF NOT EXISTS push_mirrors (
			id INTEGER PRIMARY KEY,
			repo_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			url TEXT NOT NULL,
			branch TEXT NOT NULL DEFAULT 'main',
			token TEXT NOT NULL DEFAULT '',
			last_rev TEXT NOT NULL DEFAULT '',
			last_error TEXT NOT NULL DEFAULT '',
			UNIQUE(repo_id, name)
		)`,
		`CREATE TABLE IF NOT EXISTS objects (
			sha TEXT PRIMARY KEY,
			kind INTEGER NOT NULL,
			content BLOB NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS snapshots (
			repo_id INTEGER NOT NULL,
			sha TEXT NOT NULL,
			revision_id TEXT NOT NULL,
			tree_id TEXT NOT NULL,
			commit_time INTEGER NOT NULL,
			meta BLOB NOT NULL,
			PRIMARY KEY(repo_id, sha)
		)`,
		`CREATE TABLE IF NOT EXISTS revisions (
			repo_id INTEGER NOT NULL,
			id TEXT NOT NULL,
			hash TEXT NOT NULL,
			created INTEGER NOT NULL,
			fork_from TEXT,
			changed_paths BLOB,
			PRIMARY KEY(repo_id, id)
		)`,
		`CREATE TABLE IF NOT EXISTS refs (
			repo_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			kind TEXT NOT NULL,
			target TEXT NOT NULL,
			PRIMARY KEY(repo_id, name)
		)`,
		`CREATE TABLE IF NOT EXISTS workspaces (
			id INTEGER PRIMARY KEY,
			path TEXT UNIQUE,
			repo_id INTEGER NOT NULL,
			current_revision TEXT,
			branch TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS remotes (
			repo_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			url TEXT NOT NULL,
			token TEXT,
			last_sync_tip TEXT NOT NULL DEFAULT '',
			default_branch TEXT NOT NULL DEFAULT '',
			ancestry JSON,
			PRIMARY KEY(repo_id, name)
		)`,
		`CREATE TABLE IF NOT EXISTS remote_refs (
			repo_id INTEGER NOT NULL,
			remote_name TEXT NOT NULL,
			kind TEXT NOT NULL,
			name TEXT NOT NULL,
			target TEXT NOT NULL,
			PRIMARY KEY(repo_id, remote_name, kind, name)
		)`,
		// ---- Lab (hosting) entities ----
		`CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY,
			username TEXT NOT NULL UNIQUE,
			display_name TEXT NOT NULL DEFAULT '',
			created INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS tokens (
			id INTEGER PRIMARY KEY,
			token TEXT NOT NULL UNIQUE,
			user_id INTEGER NOT NULL,
			level TEXT NOT NULL DEFAULT 'write',
			created INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS namespace_members (
			namespace TEXT NOT NULL,
			user_id INTEGER NOT NULL,
			role TEXT NOT NULL DEFAULT 'member',
			PRIMARY KEY(namespace, user_id)
		)`,
		`CREATE TABLE IF NOT EXISTS merge_requests (
			id INTEGER PRIMARY KEY,
			repo_id INTEGER NOT NULL,
			iid INTEGER NOT NULL,
			title TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL,
			target TEXT NOT NULL,
			state TEXT NOT NULL DEFAULT 'open',
			author_id INTEGER,
			created INTEGER NOT NULL,
			updated INTEGER NOT NULL,
			UNIQUE(repo_id, iid)
		)`,
		`CREATE TABLE IF NOT EXISTS mr_reviews (
			id INTEGER PRIMARY KEY,
			mr_id INTEGER NOT NULL,
			reviewer_id INTEGER,
			state TEXT NOT NULL,
			body TEXT NOT NULL DEFAULT '',
			created INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS mr_comments (
			id INTEGER PRIMARY KEY,
			mr_id INTEGER NOT NULL,
			author_id INTEGER,
			body TEXT NOT NULL,
			path TEXT,
			created INTEGER NOT NULL
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.d.exec(stmt); err != nil {
			return err
		}
	}
	// Lightweight additive migrations for repository metadata columns that may
	// be missing on a database created before the Lab layer existed.
	s.migrateRepoColumns()
	s.migrateRemoteColumns()
	return nil
}

// migrateRemoteColumns adds collaboration sync columns to the remotes table for
// databases created by an earlier version.
func (s *CentralStore) migrateRemoteColumns() {
	if s.d.kind != KindSQLite {
		return
	}
	columns := map[string]string{
		"last_sync_tip":  `ALTER TABLE remotes ADD COLUMN last_sync_tip TEXT NOT NULL DEFAULT ''`,
		"default_branch": `ALTER TABLE remotes ADD COLUMN default_branch TEXT NOT NULL DEFAULT ''`,
	}
	for name, ddl := range columns {
		exists := false
		rows, err := s.d.query("PRAGMA table_info(remotes)")
		if err != nil {
			continue
		}
		for rows.Next() {
			var cid int
			var cname, ctype string
			var notnull, pk int
			var dflt any
			if err := rows.Scan(&cid, &cname, &ctype, &notnull, &dflt, &pk); err == nil && cname == name {
				exists = true
			}
		}
		rows.Close()
		if !exists {
			_, _ = s.d.exec(ddl)
		}
	}
}

// migrateRepoColumns adds Lab metadata columns to the repositories table for
// databases created by an earlier version, while leaving existing rows intact.
func (s *CentralStore) migrateRepoColumns() {
	// PRAGMA is SQLite-only. For Postgres we rely on CREATE TABLE IF NOT EXISTS
	// carrying the columns already; an ALTER-backed check for pg would query
	// information_schema. Keep it simple: sqlite-only column migration.
	if s.d.kind != KindSQLite {
		return
	}
	columns := map[string]string{
		"description":       `ALTER TABLE repositories ADD COLUMN description TEXT NOT NULL DEFAULT ''`,
		"visibility":        `ALTER TABLE repositories ADD COLUMN visibility TEXT NOT NULL DEFAULT 'public'`,
		"default_branch":    `ALTER TABLE repositories ADD COLUMN default_branch TEXT NOT NULL DEFAULT 'main'`,
		"kind":              `ALTER TABLE repositories ADD COLUMN kind TEXT NOT NULL DEFAULT 'normal'`,
		"mirror_url":        `ALTER TABLE repositories ADD COLUMN mirror_url TEXT NOT NULL DEFAULT ''`,
		"mirror_branch":     `ALTER TABLE repositories ADD COLUMN mirror_branch TEXT NOT NULL DEFAULT 'main'`,
		"mirror_interval":   `ALTER TABLE repositories ADD COLUMN mirror_interval INTEGER NOT NULL DEFAULT 300`,
		"mirror_last_rev":   `ALTER TABLE repositories ADD COLUMN mirror_last_rev TEXT NOT NULL DEFAULT ''`,
		"mirror_last_sync":  `ALTER TABLE repositories ADD COLUMN mirror_last_sync INTEGER NOT NULL DEFAULT 0`,
		"mirror_last_error": `ALTER TABLE repositories ADD COLUMN mirror_last_error TEXT NOT NULL DEFAULT ''`,
		"mirror_token":      `ALTER TABLE repositories ADD COLUMN mirror_token TEXT NOT NULL DEFAULT ''`,
	}
	for name, ddl := range columns {
		exists := false
		rows, err := s.d.query("PRAGMA table_info(repositories)")
		if err != nil {
			continue
		}
		for rows.Next() {
			var cid int
			var cname, ctype string
			var notnull, pk int
			var dflt any
			if err := rows.Scan(&cid, &cname, &ctype, &notnull, &dflt, &pk); err == nil && cname == name {
				exists = true
			}
		}
		rows.Close()
		if !exists {
			_, _ = s.d.exec(ddl)
		}
	}
}

// Close closes the underlying database.
func (s *CentralStore) Close() error { return s.d.db.Close() }

// Create creates a new repository and returns a repo-scoped handle.
func (s *CentralStore) Create(r RepoRef) (*Repo, error) {
	// Check for conflict.
	var exists int
	err := s.d.queryRow("SELECT 1 FROM repositories WHERE namespace=? AND name=?", r.Namespace, r.Name).Scan(&exists)
	if err == nil {
		return nil, fmt.Errorf("%w: %s", ErrRepoExists, r)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	res, err := s.d.exec(
		"INSERT INTO repositories(namespace, name, created) VALUES(?,?,?)",
		r.Namespace, r.Name, time.Now().UTC().UnixMilli(),
	)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &Repo{cs: s, repoID: id, Namespace: r.Namespace, Name: r.Name}, nil
}

// OpenRepo opens an existing repository. Returns ErrRepoNotFound if missing.
func (s *CentralStore) OpenRepo(r RepoRef) (*Repo, error) {
	var id int64
	err := s.d.queryRow("SELECT id FROM repositories WHERE namespace=? AND name=?", r.Namespace, r.Name).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: %s", ErrRepoNotFound, r)
	}
	if err != nil {
		return nil, err
	}
	return &Repo{cs: s, repoID: id, Namespace: r.Namespace, Name: r.Name}, nil
}

// RepoExists reports whether a repository exists.
func (s *CentralStore) RepoExists(r RepoRef) (bool, error) {
	var one int
	err := s.d.queryRow("SELECT 1 FROM repositories WHERE namespace=? AND name=?", r.Namespace, r.Name).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Delete removes a repository and its scoped metadata.
func (s *CentralStore) Delete(r RepoRef) error {
	repo, err := s.OpenRepo(r)
	if err != nil {
		return err
	}
	for _, table := range []string{"snapshots", "revisions", "refs", "merge_requests"} {
		if _, err := s.d.exec("DELETE FROM "+table+" WHERE repo_id=?", repo.repoID); err != nil {
			return err
		}
	}
	// Releases cascade to assets; delete any MR reviews/comments too.
	for _, table := range []string{"mr_reviews", "mr_comments"} {
		if _, err := s.d.exec("DELETE FROM "+table+" WHERE mr_id IN (SELECT id FROM merge_requests WHERE repo_id=?)", repo.repoID); err != nil {
			return err
		}
	}
	_, err = s.d.exec("DELETE FROM repositories WHERE id=?", repo.repoID)
	return err
}

// List returns all repositories sorted by namespace/name.
func (s *CentralStore) List() ([]RepoRef, error) {
	rows, err := s.d.query("SELECT namespace, name FROM repositories ORDER BY namespace, name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RepoRef
	for rows.Next() {
		var r RepoRef
		if err := rows.Scan(&r.Namespace, &r.Name); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Fork copies a repository's scoped data (snapshots, revisions, refs) plus its
// hosting metadata into a new (namespace, name), sharing the global object
// store. The source repository is left untouched. It returns the new repo.
func (s *CentralStore) Fork(src RepoRef, dst RepoRef) (*Repo, error) {
	srcRepo, err := s.OpenRepo(src)
	if err != nil {
		return nil, err
	}
	dstRepo, err := s.Create(dst)
	if err != nil {
		return nil, err
	}
	// Copy hosting metadata.
	if meta, err := srcRepo.RepoMeta(); err == nil {
		_ = dstRepo.UpdateRepoMeta(meta)
	}
	// Copy revisions and snapshots, remapping the repo_id.
	snapRows, err := s.d.query(
		"SELECT sha, revision_id, tree_id, commit_time, meta FROM snapshots WHERE repo_id=?",
		srcRepo.repoID,
	)
	if err != nil {
		return nil, err
	}
	for snapRows.Next() {
		var sha, revisionID, treeID string
		var commitTime int64
		var meta []byte
		if err := snapRows.Scan(&sha, &revisionID, &treeID, &commitTime, &meta); err != nil {
			snapRows.Close()
			return nil, err
		}
		if _, err := s.d.exec(
			"INSERT INTO snapshots(repo_id, sha, revision_id, tree_id, commit_time, meta) VALUES(?,?,?,?,?,?) "+
				"ON CONFLICT(repo_id, sha) DO NOTHING",
			dstRepo.repoID, sha, revisionID, treeID, commitTime, meta,
		); err != nil {
			snapRows.Close()
			return nil, err
		}
	}
	snapRows.Close()
	revRows, err := s.d.query(
		"SELECT id, hash, created, fork_from, changed_paths FROM revisions WHERE repo_id=?",
		srcRepo.repoID,
	)
	if err != nil {
		return nil, err
	}
	for revRows.Next() {
		var id, hash string
		var created int64
		var forkFrom sql.NullString
		var changed []byte
		if err := revRows.Scan(&id, &hash, &created, &forkFrom, &changed); err != nil {
			revRows.Close()
			return nil, err
		}
		if _, err := s.d.exec(
			"INSERT INTO revisions(repo_id, id, hash, created, fork_from, changed_paths) VALUES(?,?,?,?,?,?) "+
				"ON CONFLICT(repo_id, id) DO NOTHING",
			dstRepo.repoID, id, hash, created, forkFrom, changed,
		); err != nil {
			revRows.Close()
			return nil, err
		}
	}
	revRows.Close()
	refRows, err := s.d.query(
		"SELECT name, kind, target FROM refs WHERE repo_id=?",
		srcRepo.repoID,
	)
	if err != nil {
		return nil, err
	}
	for refRows.Next() {
		var name, kind, target string
		if err := refRows.Scan(&name, &kind, &target); err != nil {
			refRows.Close()
			return nil, err
		}
		if _, err := s.d.exec(
			"INSERT INTO refs(repo_id, name, kind, target) VALUES(?,?,?,?) ON CONFLICT(repo_id, name) DO NOTHING",
			dstRepo.repoID, name, kind, target,
		); err != nil {
			refRows.Close()
			return nil, err
		}
	}
	refRows.Close()
	return dstRepo, nil
}

// RepoID returns the numeric repo id used internally.
func (r *Repo) RepoID() int64 { return r.repoID }

func (r *Repo) String() string { return r.Namespace + "/" + r.Name }

// WriteObject stores a global content-addressed object. Objects are immutable,
// so an existing object is never overwritten.
func (r *Repo) WriteObject(o *object.Object) error {
	id := o.ID()
	payload, err := object.EncodeObject(o)
	if err != nil {
		return err
	}
	_, err = r.cs.d.exec(
		"INSERT INTO objects(sha, kind, content) VALUES(?,?,?) ON CONFLICT(sha) DO NOTHING",
		id.String(), int(o.Kind), payload,
	)
	return err
}

// WriteObjectsBatch stores many objects in a single transaction, reducing
// round-trips when applying a bundle. Each object's id is recomputed from its
// content; duplicate ids are ignored (idempotent).
func (r *Repo) WriteObjectsBatch(objs []*object.Object) error {
	if len(objs) == 0 {
		return nil
	}
	tx, err := r.cs.d.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(
		"INSERT INTO objects(sha, kind, content) VALUES(?,?,?) ON CONFLICT(sha) DO NOTHING",
	)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, o := range objs {
		id := o.ID()
		payload, err := object.EncodeObject(o)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		if _, err := stmt.Exec(id.String(), int(o.Kind), payload); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// ReadObject reads a global content-addressed object.
func (r *Repo) ReadObject(id object.ID) (*object.Object, error) {
	var kind int
	var content []byte
	err := r.cs.d.queryRow("SELECT kind, content FROM objects WHERE sha=?", id.String()).Scan(&kind, &content)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return object.DecodeObject(id, content)
}

// ObjectIDs returns all content-addressed object ids held anywhere in the
// central store (objects are global and deduplicated across repositories). It
// is used by clients to advertise "have" objects during fetch/push so a peer
// only sends objects the destination lacks.
func (r *Repo) ObjectIDs() ([]object.ID, error) {
	rows, err := r.cs.d.query("SELECT sha FROM objects")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []object.ID
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		id, err := object.HexToID(s)
		if err != nil {
			continue
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ObjectExists reports whether an object is present.
func (r *Repo) ObjectExists(id object.ID) (bool, error) {
	var one int
	err := r.cs.d.queryRow("SELECT 1 FROM objects WHERE sha=?", id.String()).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// PutSnapshot stores a snapshot scoped to this repository. Parents, author, and
// description are packed into a single compact meta BLOB (see encoding pkg).
func (r *Repo) PutSnapshot(snap *Snapshot) error {
	meta := encoding.EncodeSnapshotMeta(encoding.BaseSnapshot{
		Parents:     snap.Parents,
		Author:      encoding.Author{Name: snap.Author.Name, Email: snap.Author.Email},
		Description: snap.Description,
	})
	_, err := r.cs.d.exec(
		`INSERT INTO snapshots(repo_id, sha, revision_id, tree_id, commit_time, meta)
		 VALUES(?,?,?,?,?,?)
		 ON CONFLICT(repo_id, sha) DO UPDATE SET revision_id=excluded.revision_id, tree_id=excluded.tree_id, commit_time=excluded.commit_time, meta=excluded.meta`,
		r.repoID, snap.RevisionHash.String(), snap.RevisionID, snap.TreeID.String(),
		snap.CommitTime.UnixMilli(), meta,
	)
	return err
}

// GetSnapshot reads a snapshot scoped to this repository.
func (r *Repo) GetSnapshot(id object.ID) (*Snapshot, error) {
	return r.getSnapshot(id)
}

func (r *Repo) getSnapshot(id object.ID) (*Snapshot, error) {
	var sha, revisionID, treeID string
	var commitTime int64
	var meta []byte
	err := r.cs.d.queryRow(
		"SELECT sha, revision_id, tree_id, commit_time, meta FROM snapshots WHERE repo_id=? AND sha=?",
		r.repoID, id.String(),
	).Scan(&sha, &revisionID, &treeID, &commitTime, &meta)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	oid, err := object.HexToID(sha)
	if err != nil {
		return nil, err
	}
	tid, err := object.HexToID(treeID)
	if err != nil {
		return nil, err
	}
	decoded, err := encoding.DecodeSnapshotMeta(meta)
	if err != nil {
		return nil, err
	}
	return &Snapshot{
		RevisionHash: oid, RevisionID: revisionID, Parents: decoded.Parents, TreeID: tid,
		Description: decoded.Description,
		Author:      Author{Name: decoded.Author.Name, Email: decoded.Author.Email},
		CommitTime:  time.UnixMilli(commitTime),
	}, nil
}

// PutRevision stores a revision scoped to this repository.
func (r *Repo) PutRevision(rev *Revision) error {
	changed, err := json.Marshal(rev.ChangedPaths)
	if err != nil {
		return err
	}
	_, err = r.cs.d.exec(
		`INSERT INTO revisions(repo_id, id, hash, created, fork_from, changed_paths) VALUES(?,?,?,?,?,?)
		 ON CONFLICT(repo_id, id) DO UPDATE SET hash=excluded.hash, fork_from=excluded.fork_from, changed_paths=excluded.changed_paths`,
		r.repoID, rev.ID, rev.Hash.String(), rev.Created.UnixMilli(), nullable(rev.ForkFrom), changed,
	)
	return err
}

// PutRevisionTx inserts a revision within an open transaction.
func (r *Repo) PutRevisionTx(tx *sql.Tx, rev *Revision) error {
	changed, err := json.Marshal(rev.ChangedPaths)
	if err != nil {
		return err
	}
	_, err = r.cs.d.execTx(tx,
		`INSERT INTO revisions(repo_id, id, hash, created, fork_from, changed_paths) VALUES(?,?,?,?,?,?)
		 ON CONFLICT(repo_id, id) DO UPDATE SET hash=excluded.hash, fork_from=excluded.fork_from, changed_paths=excluded.changed_paths`,
		r.repoID, rev.ID, rev.Hash.String(), rev.Created.UnixMilli(), nullable(rev.ForkFrom), changed,
	)
	return err
}

// PutSnapshotTx inserts a snapshot within an open transaction.
func (r *Repo) PutSnapshotTx(tx *sql.Tx, snap *Snapshot) error {
	meta := encoding.EncodeSnapshotMeta(encoding.BaseSnapshot{
		Parents:     snap.Parents,
		Author:      encoding.Author{Name: snap.Author.Name, Email: snap.Author.Email},
		Description: snap.Description,
	})
	_, err := r.cs.d.execTx(tx,
		`INSERT INTO snapshots(repo_id, sha, revision_id, tree_id, commit_time, meta)
		 VALUES(?,?,?,?,?,?)
		 ON CONFLICT(repo_id, sha) DO UPDATE SET revision_id=excluded.revision_id, tree_id=excluded.tree_id, commit_time=excluded.commit_time, meta=excluded.meta`,
		r.repoID, snap.RevisionHash.String(), snap.RevisionID, snap.TreeID.String(),
		snap.CommitTime.UnixMilli(), meta,
	)
	return err
}

// WriteObjectsBatchTx stores objects in the given transaction.
func (r *Repo) WriteObjectsBatchTx(tx *sql.Tx, objs []*object.Object) error {
	for _, o := range objs {
		id := o.ID()
		payload, err := object.EncodeObject(o)
		if err != nil {
			return err
		}
		if _, err := r.cs.d.execTx(tx,
			"INSERT INTO objects(sha, kind, content) VALUES(?,?,?) ON CONFLICT(sha) DO NOTHING",
			id.String(), int(o.Kind), payload,
		); err != nil {
			return err
		}
	}
	return nil
}

// BeginTx starts a transaction on the central store.
func (r *Repo) BeginTx() (*sql.Tx, error) { return r.cs.d.db.Begin() }

// GetRevision reads a revision scoped to this repository.
func (r *Repo) GetRevision(id string) (*Revision, error) {
	var hash string
	var created int64
	var forkFrom sql.NullString
	var changed []byte
	err := r.cs.d.queryRow(
		"SELECT hash, created, fork_from, changed_paths FROM revisions WHERE repo_id=? AND id=?",
		r.repoID, id,
	).Scan(&hash, &created, &forkFrom, &changed)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	cur, _ := idFromStr(hash)
	var paths []string
	if err := json.Unmarshal(changed, &paths); err != nil {
		paths = nil
	}
	return &Revision{ID: id, Hash: cur, Created: time.UnixMilli(created), ForkFrom: forkFrom.String, ChangedPaths: paths}, nil
}

// UpdateRevisionHash atomically repoints a revision's Hash field. It preserves
// the existing ChangedPaths.
func (r *Repo) UpdateRevisionHash(revisionID string, hash object.ID) error {
	rev, err := r.GetRevision(revisionID)
	if err != nil {
		return err
	}
	rev.Hash = hash
	return r.PutRevision(rev)
}

// ListRevisions lists revisions scoped to this repository.
func (r *Repo) ListRevisions() ([]*Revision, error) {
	rows, err := r.cs.d.query(
		"SELECT id, hash, created, fork_from, changed_paths FROM revisions WHERE repo_id=?",
		r.repoID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Revision
	for rows.Next() {
		var id, hash string
		var created int64
		var forkFrom sql.NullString
		var changed []byte
		if err := rows.Scan(&id, &hash, &created, &forkFrom, &changed); err != nil {
			return nil, err
		}
		cur, _ := idFromStr(hash)
		var paths []string
		if err := json.Unmarshal(changed, &paths); err != nil {
			paths = nil
		}
		out = append(out, &Revision{ID: id, Hash: cur, Created: time.UnixMilli(created), ForkFrom: forkFrom.String, ChangedPaths: paths})
	}
	return out, rows.Err()
}

// PutRef stores a ref scoped to this repository.
func (r *Repo) PutRef(ref *Ref) error {
	_, err := r.cs.d.exec(
		`INSERT INTO refs(repo_id, name, kind, target) VALUES(?,?,?,?)
		 ON CONFLICT(repo_id, name) DO UPDATE SET kind=excluded.kind, target=excluded.target`,
		r.repoID, ref.Name, string(ref.Kind), ref.Target,
	)
	return err
}

// DeleteRef removes a ref scoped to this repository.
func (r *Repo) DeleteRef(name string) error {
	res, err := r.cs.d.exec("DELETE FROM refs WHERE repo_id=? AND name=?", r.repoID, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetRef reads a ref scoped to this repository.
func (r *Repo) GetRef(name string) (*Ref, error) {
	var kind, target string
	err := r.cs.d.queryRow("SELECT kind, target FROM refs WHERE repo_id=? AND name=?", r.repoID, name).Scan(&kind, &target)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &Ref{Name: name, Kind: RefKind(kind), Target: target}, nil
}

// ListRefs lists refs scoped to this repository.
func (r *Repo) ListRefs() ([]*Ref, error) {
	rows, err := r.cs.d.query("SELECT name, kind, target FROM refs WHERE repo_id=?", r.repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Ref
	for rows.Next() {
		var rf Ref
		if err := rows.Scan(&rf.Name, &rf.Kind, &rf.Target); err != nil {
			return nil, err
		}
		out = append(out, &rf)
	}
	return out, rows.Err()
}

// Close is a no-op for the repo handle (the CentralStore is shared).
func (r *Repo) Close() error { return nil }

// RepoRef returns the RepoRef for this handle.
func (r *Repo) RepoRef() RepoRef { return RepoRef{Namespace: r.Namespace, Name: r.Name} }

// Remote is a named URL to another EasyVCS server. Token is the optional
// bearer token used to authenticate writes against that server.
type Remote struct {
	Name  string
	URL   string
	Token string
}

// PutRemote registers or updates a remote for this repository. An empty token
// clears the stored token.
func (r *Repo) PutRemote(name, url, token string) error {
	_, err := r.cs.d.exec(
		`INSERT INTO remotes(repo_id, name, url, token) VALUES(?,?,?,?)
		 ON CONFLICT(repo_id, name) DO UPDATE SET url=excluded.url, token=excluded.token`,
		r.repoID, name, url, nullable(token),
	)
	return err
}

// UpdateRemoteSyncTip records the last successfully synced tip for a remote
// branch (the local revision id that mirrors the remote's tip after a pull).
// Empty clears it. This enables incremental/conflict-aware pulls.
func (r *Repo) UpdateRemoteSyncTip(name, lastSyncTip string) error {
	_, err := r.cs.d.exec(
		"UPDATE remotes SET last_sync_tip=? WHERE repo_id=? AND name=?",
		lastSyncTip, r.repoID, name,
	)
	return err
}

// GetLastSyncTip returns the last synced tip for a remote ("" if none).
func (r *Repo) GetLastSyncTip(name string) (string, error) {
	var t sql.NullString
	err := r.cs.d.queryRow(
		"SELECT last_sync_tip FROM remotes WHERE repo_id=? AND name=?",
		r.repoID, name,
	).Scan(&t)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return t.String, nil
}

// RemoteRef is a snapshot of a remote's ref as last fetched, scoped to a remote
// name (e.g. "origin.main", kind=branch). This mirrors git's refs/remotes.
type RemoteRef struct {
	RemoteName string
	Kind       RefKind
	Name       string
	Target     string // revision id on the remote
}

// SetRemoteRef upserts a remote ref. Primary key is remote+kind+name.
func (r *Repo) SetRemoteRef(remoteName string, ref *RemoteRef) error {
	_, err := r.cs.d.exec(
		`INSERT INTO remote_refs(repo_id, remote_name, kind, name, target)
		 VALUES(?,?,?,?,?)
		 ON CONFLICT(repo_id, remote_name, kind, name) DO UPDATE SET target=excluded.target`,
		r.repoID, remoteName, string(ref.Kind), ref.Name, ref.Target,
	)
	return err
}

// ListRemoteRefs lists all remote refs for this repo (optionally filtered by
// remote name). Returns refs keyed under the remote (name is bare, e.g. "main").
func (r *Repo) ListRemoteRefs(remoteName string) ([]*RemoteRef, error) {
	q := "SELECT remote_name, kind, name, target FROM remote_refs WHERE repo_id=?"
	var args []any
	args = append(args, r.repoID)
	if remoteName != "" {
		q += " AND remote_name=?"
		args = append(args, remoteName)
	}
	q += " ORDER BY remote_name, name"
	rows, err := r.cs.d.query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*RemoteRef
	for rows.Next() {
		var rr RemoteRef
		var kind string
		if err := rows.Scan(&rr.RemoteName, &kind, &rr.Name, &rr.Target); err != nil {
			return nil, err
		}
		rr.Kind = RefKind(kind)
		out = append(out, &rr)
	}
	return out, rows.Err()
}

// DeleteRemoteRefsForRemote clears all recorded refs for a remote (used when a
// remote is deleted or force-refreshed).
func (r *Repo) DeleteRemoteRefsForRemote(remoteName string) error {
	_, err := r.cs.d.exec("DELETE FROM remote_refs WHERE repo_id=? AND remote_name=?", r.repoID, remoteName)
	return err
}

// SetRemoteDefaultBranch records which branch on a remote is its default.
func (r *Repo) SetRemoteDefaultBranch(name, def string) error {
	_, err := r.cs.d.exec(
		"UPDATE remotes SET default_branch=? WHERE repo_id=? AND name=?",
		def, r.repoID, name,
	)
	return err
}

// GetRemoteDefaultBranch returns the remote's default branch ("" if none).
func (r *Repo) GetRemoteDefaultBranch(name string) (string, error) {
	var d sql.NullString
	err := r.cs.d.queryRow(
		"SELECT default_branch FROM remotes WHERE repo_id=? AND name=?",
		r.repoID, name,
	).Scan(&d)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return d.String, nil
}

// GetRemote returns a remote by name.
func (r *Repo) GetRemote(name string) (*Remote, error) {
	var url string
	var token sql.NullString
	err := r.cs.d.queryRow(
		"SELECT url, token FROM remotes WHERE repo_id=? AND name=?",
		r.repoID, name,
	).Scan(&url, &token)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &Remote{Name: name, URL: url, Token: token.String}, nil
}

// DeleteRemote removes a remote by name.
func (r *Repo) DeleteRemote(name string) error {
	_, err := r.cs.d.exec("DELETE FROM remotes WHERE repo_id=? AND name=?", r.repoID, name)
	return err
}

// ListRemotes lists all remotes for this repository. Tokens are omitted from
// the returned list to avoid leaking secrets.
func (r *Repo) ListRemotes() ([]*Remote, error) {
	rows, err := r.cs.d.query("SELECT name, url FROM remotes WHERE repo_id=?", r.repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Remote
	for rows.Next() {
		var rem Remote
		if err := rows.Scan(&rem.Name, &rem.URL); err != nil {
			return nil, err
		}
		out = append(out, &rem)
	}
	return out, rows.Err()
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// QueryCount returns the total number of content-addressed objects. It is a
// convenience for tests to verify global deduplication.
func (s *CentralStore) QueryCount(count *int) error {
	return s.d.queryRow("SELECT COUNT(*) FROM objects").Scan(count)
}

func idsToStrs(ids []object.ID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

func idFromStr(s string) (object.ID, error) {
	return object.HexToID(s)
}

func privatePlaceholder() string { return strings.TrimSpace(" ") }
