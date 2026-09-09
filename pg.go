package repoext

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// PgConfig kept for signature compatibility but now names a local SQLite file.
// The repo-extension's private store (bookmark<->session mapping) lives in
// one file; this avoids any external Postgres dependency.
type PgConfig struct {
	Host     string
	Port     string
	User     string
	Password string
	DB       string
}

func (c PgConfig) dsn(_ string) string { return c.DB }

// MapRow is one row of the bookmark<->session mapping table.
type MapRow struct {
	Org         string
	Repo        string
	Bookmark    string
	SessionName string
}

// Store is the repo-extension persistence layer over a SQLite database.
type Store struct {
	db *sql.DB
}

// OpenStore opens (and migrates) the SQLite database at cfg.DB.
func OpenStore(_ context.Context, cfg PgConfig) (*Store, error) {
	path := cfg.DB
	if path == "" {
		path = filepath.Join(os.TempDir(), "easylab-repoext.db")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	gdb, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("sqlite open: %w", err)
	}
	sdb, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("sqlite db: %w", err)
	}
	sdb.SetMaxOpenConns(1)
	st := &Store{db: sdb}
	if err := st.migrate(context.Background()); err != nil {
		sdb.Close()
		return nil, err
	}
	return st, nil
}

func (s *Store) Close() { if s != nil && s.db != nil { _ = s.db.Close() } }

func (s *Store) migrate(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS managed_repos (
  org        TEXT NOT NULL,
  repo       TEXT NOT NULL,
  created_at INTEGER NOT NULL DEFAULT (unixepoch()),
  PRIMARY KEY (org, repo)
);
CREATE TABLE IF NOT EXISTS session_repos (
  org          TEXT NOT NULL,
  repo         TEXT NOT NULL,
  bookmark     TEXT NOT NULL,
  session_name TEXT NOT NULL UNIQUE,
  created_at   INTEGER NOT NULL DEFAULT (unixepoch()),
  updated_at   INTEGER NOT NULL DEFAULT (unixepoch()),
  PRIMARY KEY (org, repo, bookmark)
);
`
	_, err := s.db.ExecContext(ctx, ddl)
	if err != nil {
		return fmt.Errorf("ddl: %w", err)
	}
	return nil
}

// ---- managed repos ----

func (s *Store) IsManaged(ctx context.Context, org, repo string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM managed_repos WHERE org=? AND repo=?`, org, repo).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) InsertManaged(ctx context.Context, org, repo string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO managed_repos (org, repo) VALUES (?, ?) ON CONFLICT DO NOTHING`, org, repo)
	return err
}

func (s *Store) DeleteManaged(ctx context.Context, org, repo string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM managed_repos WHERE org=? AND repo=?`, org, repo)
	return err
}

func (s *Store) ListManaged(ctx context.Context) ([]MapRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT org, repo FROM managed_repos ORDER BY org, repo`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MapRow
	for rows.Next() {
		var r MapRow
		if err := rows.Scan(&r.Org, &r.Repo); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- mapping rows ----

const mapCols = `org, repo, bookmark, session_name`

func scanMapRowFrom(row *sql.Row) (*MapRow, error) {
	var r MapRow
	err := row.Scan(&r.Org, &r.Repo, &r.Bookmark, &r.SessionName)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *Store) GetRow(ctx context.Context, org, repo, bookmark string) (*MapRow, error) {
	return scanMapRowFrom(s.db.QueryRowContext(ctx,
		`SELECT `+mapCols+` FROM session_repos WHERE org=? AND repo=? AND bookmark=?`,
		org, repo, bookmark))
}

func (s *Store) GetRowBySession(ctx context.Context, sessionName string) (*MapRow, error) {
	return scanMapRowFrom(s.db.QueryRowContext(ctx,
		`SELECT `+mapCols+` FROM session_repos WHERE session_name=?`, sessionName))
}

// InsertRow records a mapping. A unique violation is returned as errConflict so
// callers can surface 409.
func (s *Store) InsertRow(ctx context.Context, org, repo, bookmark, sessionName string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO session_repos (org, repo, bookmark, session_name) VALUES (?, ?, ?, ?)`,
		org, repo, bookmark, sessionName)
	if isUniqueViolation(err) {
		return errConflict("bookmark or session already bound")
	}
	return err
}

// RenameRow moves a mapping row to a new bookmark + session name.
func (s *Store) RenameRow(ctx context.Context, org, repo, fromBM, toBM, toSession string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE session_repos SET bookmark=?, session_name=?, updated_at=unixepoch()
		 WHERE org=? AND repo=? AND bookmark=?`,
		toBM, toSession, org, repo, fromBM)
	if err != nil {
		if isUniqueViolation(err) {
			return errConflict("target bookmark or session already bound")
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound("mapping row not found")
	}
	return nil
}

func (s *Store) DeleteRow(ctx context.Context, org, repo, bookmark string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM session_repos WHERE org=? AND repo=? AND bookmark=?`, org, repo, bookmark)
	return err
}

// DeleteRowsForRepo removes every mapping row of one repo (delete-repo path).
func (s *Store) DeleteRowsForRepo(ctx context.Context, org, repo string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM session_repos WHERE org=? AND repo=?`, org, repo)
	return err
}

func (s *Store) ListRowsForRepo(ctx context.Context, org, repo string) ([]MapRow, error) {
	return s.listRows(ctx,
		`SELECT `+mapCols+` FROM session_repos WHERE org=? AND repo=? ORDER BY bookmark`, org, repo)
}

func (s *Store) ListRows(ctx context.Context) ([]MapRow, error) {
	return s.listRows(ctx, `SELECT `+mapCols+` FROM session_repos ORDER BY org, repo, bookmark`)
}

func (s *Store) listRows(ctx context.Context, q string, args ...interface{}) ([]MapRow, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MapRow
	for rows.Next() {
		var r MapRow
		if err := rows.Scan(&r.Org, &r.Repo, &r.Bookmark, &r.SessionName); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- error helpers ----

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return containsStr(err.Error(), "UNIQUE constraint failed")
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
