package store

import "testing"

func TestRebindPostgres(t *testing.T) {
	q := "INSERT INTO t(a,b) VALUES(?,?) WHERE c=? AND d IS NOT NULL"
	got := rebindPostgres(q)
	want := "INSERT INTO t(a,b) VALUES($1,$2) WHERE c=$3 AND d IS NOT NULL"
	if got != want {
		t.Fatalf("rebind: %q want %q", got, want)
	}
}

func TestOpenDriverSQLite(t *testing.T) {
	cs, err := OpenDriver(DriverConfig{Kind: KindSQLite, DSN: t.TempDir() + "/t.db"})
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	// Repo create/list round-trip.
	r, err := cs.Create(RepoRef{Namespace: "n", Name: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if r.String() != "n/r" {
		t.Fatalf("repo: %s", r.String())
	}
}
