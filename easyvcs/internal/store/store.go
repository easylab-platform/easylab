// Package store defines the persistent storage boundary for EasyVCS.
//
// EasyVCS uses a single central SQL database (default ~/.easyvcs/easyvcs.db,
// overridable via EASYVCS_HOME). All repositories live in that one database,
// keyed by a (namespace, name) pair. Content-addressed objects are global and
// deduplicated across repositories; revisions, snapshots, and refs are scoped
// to a repository via a repo id.
//
// The semantic layer depends on the repo-scoped RepoStore interface, never on
// a concrete backend. This lets the same code run against SQLite (local) or
// Postgres (server) with no changes, and no cgo.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"easyvcs/internal/object"
)

// ErrNotFound is returned when a keyed entity is absent.
var ErrNotFound = errors.New("store: not found")

// ErrRepoExists is returned when trying to create a repository that already exists.
var ErrRepoExists = errors.New("store: repository already exists")

// ErrRepoNotFound is returned when a repository is absent.
var ErrRepoNotFound = errors.New("store: repository not found")

// DefaultDBFile is the file name of the central database inside the home dir.
const DefaultDBFile = "easyvcs.db"

// RepoRef uniquely identifies a repository within the central store.
type RepoRef struct {
	Namespace string
	Name      string
}

func (r RepoRef) String() string { return r.Namespace + "/" + r.Name }

// Author is a named identity.
type Author struct {
	Name  string
	Email string
}

// String renders an author as "Name <email>".
func (a Author) String() string {
	if a.Email == "" {
		return a.Name
	}
	return fmt.Sprintf("%s <%s>", a.Name, a.Email)
}

// Snapshot is the content-addressed view of a revision: it is the graph node.
// A Snapshot belongs to exactly one Revision (1:1), so its hash changes on
// rewrite (rebase) while the revision_id stays stable.
type Snapshot struct {
	// RevisionHash is the content-addressed id (sha) of this snapshot. It is
	// mutable: it changes whenever the revision is rewritten.
	RevisionHash object.ID
	// RevisionID is the stable identity this snapshot belongs to (never
	// changes). It is a random stable id (hex), independent of content.
	RevisionID string
	// Parents contains the ids of the parent snapshots (DAG edges).
	Parents []object.ID
	// TreeID is the root tree of this snapshot.
	TreeID object.ID
	// Description is the commit message.
	Description string
	// Author identity.
	Author Author
	// CommitTime is the creation time.
	CommitTime time.Time
}

// Revision is the stable, user-facing unit of work. It holds a pointer to its
// single current snapshot. The revision_id never changes; the pointed-to
// snapshot's hash does change as the revision is rewritten. Revision and
// Snapshot are strictly 1:1: a revision always points at one current snapshot,
// and rewriting replaces that snapshot entirely (old object remains in the
// content-addressed store if still referenced).
type Revision struct {
	// ID is the stable revision id (hex). Generated once and never modified.
	ID string
	// Hash points at the current snapshot's revision_hash.
	Hash object.ID
	// Created time.
	Created time.Time
	// ForkFrom records the revision id this revision was forked from, if any.
	// Used by workspace auto-fork so the origin is traceable.
	ForkFrom string
	// ChangedPaths lists the repo-relative paths whose content changed in this
	// revision relative to its parent (or all paths for the root revision). It
	// is used to cheaply answer "which revisions modified a given file" by
	// filtering the DAG walk without re-reading every tree.
	ChangedPaths []string
}

// RefKind distinguishes mutable branchs from immutable tags.
type RefKind string

// Ref kinds.
const (
	RefBranch RefKind = "branch"
	RefTag    RefKind = "tag"
)

// Ref is a named pointer to a revision.
type Ref struct {
	Name   string
	Kind   RefKind
	Target string // revision id
}

// RepoStore is the repo-scoped persistence boundary used by the semantic layer.
// Object primitives are content-addressed and global; the metadata methods are
// scoped to a single repository.
type RepoStore interface {
	// Object access (content-addressed, immutable, global).
	WriteObject(o *object.Object) error
	ReadObject(id object.ID) (*object.Object, error)
	ObjectExists(id object.ID) (bool, error)

	// Snapshot access.
	PutSnapshot(s *Snapshot) error
	GetSnapshot(id object.ID) (*Snapshot, error)

	// Revision access.
	PutRevision(r *Revision) error
	GetRevision(id string) (*Revision, error)
	// UpdateRevisionHash atomically points a revision's Hash to a new
	// snapshot hash.
	UpdateRevisionHash(revisionID string, hash object.ID) error
	ListRevisions() ([]*Revision, error)

	// Refs.
	PutRef(r *Ref) error
	DeleteRef(name string) error
	GetRef(name string) (*Ref, error)
	ListRefs() ([]*Ref, error)

	// Transactional writes (for atomic commit from changes).
	BeginTx() (*sql.Tx, error)
	WriteObjectsBatchTx(tx *sql.Tx, objs []*object.Object) error
	PutSnapshotTx(tx *sql.Tx, s *Snapshot) error
	PutRevisionTx(tx *sql.Tx, r *Revision) error

	// Close flushes and closes the backend.
	Close() error
}

// Repo is a repo-scoped handle bound to a specific repository in the central
// store. It implements RepoStore.
type Repo struct {
	cs        *CentralStore
	repoID    int64
	Namespace string
	Name      string
}
