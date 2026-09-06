package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// Kinds of metadata backends.
const (
	KindSQLite   = "sqlite"
	KindPostgres = "postgres"
)

// DriverConfig selects the metadata backend. Kind is "sqlite" or "postgres".
type DriverConfig struct {
	Kind string
	DSN  string
}

// OpenDriver opens the central store on the configured backend. The semantic
// layer (revision/merge/transfer) is agnostic; only this boundary changes.
func OpenDriver(cfg DriverConfig) (*CentralStore, error) {
	kind := strings.ToLower(cfg.Kind)
	if kind != KindSQLite && kind != KindPostgres {
		return nil, fmt.Errorf("unsupported db driver %q (sqlite|postgres)", cfg.Kind)
	}
	dsn := cfg.DSN
	if kind == KindSQLite {
		if dsn == "" {
			dsn = DBPath()
		}
		if err := os.MkdirAll(filepath.Dir(dsn), 0o755); err != nil {
			return nil, err
		}
	}
	driverName := "sqlite"
	if kind == KindPostgres {
		driverName = "pgx"
	}
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, err
	}
	d := &sqlDialect{db: db, kind: kind}
	if kind == KindSQLite {
		d.rebind = func(q string) string { return q }
	} else {
		d.rebind = rebindPostgres
	}
	cs := &CentralStore{d: d, root: filepath.Dir(dsn)}
	if err := cs.Init(); err != nil {
		return nil, err
	}
	return cs, nil
}

// rebindPostgres rewrites `?` positional placeholders to `$1..$n`.
func rebindPostgres(q string) string {
	var b strings.Builder
	n := 0
	for _, r := range q {
		if r == '?' {
			n++
			b.WriteString(fmt.Sprintf("$%d", n))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Open opens the central store at the given SQLite file path (legacy helper).
func Open(path string) (*CentralStore, error) {
	return OpenDriver(DriverConfig{Kind: KindSQLite, DSN: path})
}

// OpenDefault opens the central store at the default SQLite path.
func OpenDefault() (*CentralStore, error) {
	return Open(DBPath())
}
