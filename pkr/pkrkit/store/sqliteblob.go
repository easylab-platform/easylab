package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"

	"github.com/pkr/pkrkit"
)

// SQLiteBlobStore stores blob bytes inline as SQLite BLOBs keyed by digest.
// It can share the same *sql.DB connection as the IndexStore, so both layers
// can live in one file. Choose this when blob totals (index docs, small
// wheels, helm charts) fit a few GB; prefer FileBlobStore for large layers.
type SQLiteBlobStore struct {
	db *sql.DB
}

// NewSQLiteBlobStore wraps an existing *sql.DB (from OpenSQLite) as a BlobStore.
func NewSQLiteBlobStore(db *sql.DB) *SQLiteBlobStore {
	return &SQLiteBlobStore{db: db}
}

// DB exposes the underlying handle so a caller can reuse it for the IndexStore.
func (s *SQLiteBlobStore) DB() *sql.DB { return s.db }

func (s *SQLiteBlobStore) ensureSchema(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS blobs (
		  digest TEXT PRIMARY KEY,
		  size   INTEGER NOT NULL,
		  data   BLOB NOT NULL
		)`)
	return err
}

// Stat implements pkrkit.BlobStore.
func (s *SQLiteBlobStore) Stat(ctx context.Context, digest string) (*int64, error) {
	if err := s.ensureSchema(ctx); err != nil {
		return nil, err
	}
	var size int64
	err := s.db.QueryRowContext(ctx, `SELECT size FROM blobs WHERE digest=?`, digest).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &size, nil
}

// Open implements pkrkit.BlobStore.
func (s *SQLiteBlobStore) Open(ctx context.Context, digest string) (io.ReadSeekCloser, error) {
	if err := s.ensureSchema(ctx); err != nil {
		return nil, err
	}
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM blobs WHERE digest=?`, digest).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return nopSeekCloser{bytes.NewReader(data)}, nil
}

// PutIfAbsent implements pkrkit.BlobStore. Bytes are read once, hashed, and
// verified against the claimed digest before insertion (dedup by digest).
func (s *SQLiteBlobStore) PutIfAbsent(ctx context.Context, digest string, r io.Reader) (bool, error) {
	if err := s.ensureSchema(ctx); err != nil {
		return false, err
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return false, err
	}
	if got := pkrkit.DigestOf(data); got != digest {
		return false, errors.New("digest mismatch")
	}
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO blobs (digest, size, data) VALUES (?,?,?)`,
		digest, len(data), data)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// HashesFor implements pkrkit.BlobStore: recompute from the stored bytes.
func (s *SQLiteBlobStore) HashesFor(ctx context.Context, digest string) (pkrkit.Hashes, error) {
	if err := s.ensureSchema(ctx); err != nil {
		return pkrkit.Hashes{}, err
	}
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM blobs WHERE digest=?`, digest).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return pkrkit.Hashes{}, pkrkit.ErrBlobUnknown
	}
	if err != nil {
		return pkrkit.Hashes{}, err
	}
	return pkrkit.ComputeHashes(bytes.NewReader(data))
}

// Delete implements pkrkit.BlobStore.
func (s *SQLiteBlobStore) Delete(ctx context.Context, digest string) error {
	if err := s.ensureSchema(ctx); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM blobs WHERE digest=?`, digest)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return pkrkit.ErrBlobUnknown
	}
	return nil
}

// List implements pkrkit.BlobStore.
func (s *SQLiteBlobStore) List(ctx context.Context) ([]string, error) {
	if err := s.ensureSchema(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT digest FROM blobs ORDER BY digest`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

type nopSeekCloser struct{ io.ReadSeeker }

func (nopSeekCloser) Close() error { return nil }
