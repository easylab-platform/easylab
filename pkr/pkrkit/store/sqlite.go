package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, ships as one binary

	"github.com/pkr/pkrkit"
)

// SQLiteStore is a metadata (IndexStore) + optional blob (BlobStore) engine
// backed by a single SQLite database. Blobs may live on the filesystem (see
// FileBlobStore) or inline as SQLite BLOBs (SQLiteBlobStore) — the metadata
// schema is the same either way, so the two can compose freely.
//
// Being one DB file: it is trivial to back with a hostPath or PVC, and it is
// the smallest unit a pull-through mirror needs to survive a pod restart.
type SQLiteStore struct {
	db *sql.DB
}

// OpenSQLite opens (and migrates) a SQLite IndexStore at path. The parent
// directory is created if needed. Use ":memory:" for a throwaway store.
func OpenSQLite(path string) (*SQLiteStore, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &SQLiteStore{db: db}, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS artifacts (
  format     TEXT NOT NULL,
  repository TEXT NOT NULL,
  version    TEXT NOT NULL,
  media_type TEXT NOT NULL DEFAULT '',
  digest     TEXT NOT NULL DEFAULT '',
  proprietary BLOB,
  blobs      TEXT NOT NULL DEFAULT '[]',
  source     TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (format, repository, version)
);
CREATE INDEX IF NOT EXISTS idx_artifacts_format ON artifacts(format);
CREATE INDEX IF NOT EXISTS idx_artifacts_repo ON artifacts(repository);

CREATE TABLE IF NOT EXISTS uploads (
  id        TEXT PRIMARY KEY,
  format    TEXT NOT NULL,
  repository TEXT NOT NULL,
  digest    TEXT NOT NULL DEFAULT '',
  bytes     INTEGER NOT NULL DEFAULT 0,
  complete  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS meta (
  format     TEXT NOT NULL,
  repository TEXT NOT NULL,
  data       BLOB,
  PRIMARY KEY (format, repository)
);
`

func migrate(db *sql.DB) error {
	_, err := db.Exec(schema)
	return err
}

// Close implements IndexStore / BlobStore.
func (s *SQLiteStore) Close() error { return s.db.Close() }

// DB exposes the underlying handle so the caller can reuse it as a BlobStore
// backend (SQLiteBlobStore).
func (s *SQLiteStore) DB() *sql.DB { return s.db }

// Put implements IndexStore.
func (s *SQLiteStore) Put(ctx context.Context, a pkrkit.Artifact) error {
	if a.Repository == "" {
		return errors.New("artifact: empty repository")
	}
	blobs, err := json.Marshal(a.Blobs)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO artifacts (format, repository, version, media_type, digest, proprietary, blobs, source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(format, repository, version) DO UPDATE SET
			media_type=excluded.media_type,
			digest=excluded.digest,
			proprietary=excluded.proprietary,
			blobs=excluded.blobs,
			source=excluded.source`,
		a.Format, a.Repository, a.Version, a.MediaType, a.Digest, a.Proprietary, blobs, a.Source)
	return err
}

// Get implements IndexStore.
func (s *SQLiteStore) Get(ctx context.Context, format, repository, version string) (pkrkit.Artifact, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT media_type, digest, proprietary, blobs, source
		FROM artifacts WHERE format=? AND repository=? AND version=?`,
		format, repository, version)
	var a pkrkit.Artifact
	var proprietary []byte
	var blobs string
	if err := row.Scan(&a.MediaType, &a.Digest, &proprietary, &blobs, &a.Source); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return pkrkit.Artifact{}, pkrkit.ErrArtifactUnknown
		}
		return pkrkit.Artifact{}, err
	}
	if err := json.Unmarshal([]byte(blobs), &a.Blobs); err != nil {
		return pkrkit.Artifact{}, err
	}
	a.Format, a.Repository, a.Version, a.Proprietary = format, repository, version, proprietary
	return a, nil
}

// Delete implements IndexStore.
func (s *SQLiteStore) Delete(ctx context.Context, format, repository, version string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM artifacts WHERE format=? AND repository=? AND version=?`,
		format, repository, version)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return pkrkit.ErrArtifactUnknown
	}
	return nil
}

// ListVersions implements IndexStore.
func (s *SQLiteStore) ListVersions(ctx context.Context, format, repository string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT version FROM artifacts WHERE format=? AND repository=? ORDER BY version`,
		format, repository)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ListRepositoriesByFormat implements IndexStore.
func (s *SQLiteStore) ListRepositoriesByFormat(ctx context.Context, format string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT repository FROM artifacts WHERE format=? ORDER BY repository`, format)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListRepositories implements IndexStore.
func (s *SQLiteStore) ListRepositories(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT repository FROM artifacts ORDER BY repository`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListPackages implements IndexStore.
func (s *SQLiteStore) ListPackages(ctx context.Context) ([]pkrkit.PackageSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT format, repository, version, media_type, digest,
		       COALESCE(CAST(JSON_ARRAY_LENGTH(blobs) AS INTEGER), 0)
		FROM artifacts ORDER BY format, repository, version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// NOTE: size is not stored as a column; we approximate with blob count.
	// A real deployment would keep a size column; kept minimal for the sketch.
	var out []pkrkit.PackageSummary
	for rows.Next() {
		var s pkrkit.PackageSummary
		var n int
		if err := rows.Scan(&s.Format, &s.Repository, &s.Version, &s.MediaType, &s.Digest, &n); err != nil {
			return nil, err
		}
		s.Size = int64(n)
		out = append(out, s)
	}
	return out, rows.Err()
}

// DeleteRepo implements IndexStore.
func (s *SQLiteStore) DeleteRepo(ctx context.Context, format, repository string) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM artifacts WHERE format=? AND repository=?`, format, repository)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// SaveUpload implements IndexStore.
func (s *SQLiteStore) SaveUpload(ctx context.Context, u pkrkit.UploadRecord) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO uploads (id, format, repository, digest, bytes, complete)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			format=excluded.format, repository=excluded.repository,
			digest=excluded.digest, bytes=excluded.bytes, complete=excluded.complete`,
		u.ID, u.Format, u.Repository, u.Digest, u.Bytes, boolToInt(u.Complete))
	return err
}

// GetUpload implements IndexStore.
func (s *SQLiteStore) GetUpload(ctx context.Context, id string) (pkrkit.UploadRecord, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, format, repository, COALESCE(digest,''), bytes, complete FROM uploads WHERE id=?`, id)
	var u pkrkit.UploadRecord
	var complete int
	if err := row.Scan(&u.ID, &u.Format, &u.Repository, &u.Digest, &u.Bytes, &complete); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return pkrkit.UploadRecord{}, pkrkit.ErrUploadUnknown
		}
		return pkrkit.UploadRecord{}, err
	}
	u.Complete = complete != 0
	return u, nil
}

// DeleteUpload implements IndexStore.
func (s *SQLiteStore) DeleteUpload(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM uploads WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return pkrkit.ErrUploadUnknown
	}
	return nil
}

// ListUploads implements IndexStore.
func (s *SQLiteStore) ListUploads(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM uploads`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// GetMeta implements IndexStore.
func (s *SQLiteStore) GetMeta(ctx context.Context, format, repository string) ([]byte, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM meta WHERE format=? AND repository=?`, format, repository).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%s/%s: %w", format, repository, pkrkit.ErrArtifactUnknown)
	}
	return data, err
}

// SetMeta implements IndexStore.
func (s *SQLiteStore) SetMeta(ctx context.Context, format, repository string, data []byte) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO meta (format, repository, data) VALUES (?, ?, ?)
		ON CONFLICT(format, repository) DO UPDATE SET data=excluded.data`, format, repository, data)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
