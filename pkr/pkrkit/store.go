package pkrkit

import (
	"context"
	"errors"
	"io"
)

// Digest is the canonical content-addressed identifier of a blob. The OCI
// form is "sha256:<hex>". The BlobStore is keyed by digest strings, so stems
// of implementations are free to use any algorithm.
const SHA256 = "sha256"

// BlobStore abstracts the CAS for immutable artifact content. The default
// implementation is a filesystem store (one file per digest); a SQLite-BLOB
// or in-memory implementation can be supplied instead. This is the seam that
// lets the engine keep large layers on disk while small index docs could live
// in SQLite.
type BlobStore interface {
	// Stat returns the size of the blob, or (nil, nil) when absent.
	Stat(ctx context.Context, digest string) (*int64, error)
	// Open returns a reader (supporting Seek when possible) or (nil, nil).
	Open(ctx context.Context, digest string) (io.ReadSeekCloser, error)
	// PutIfAbsent stores the bytes under digest; returns true if stored,
	// false if it already existed (dedup).
	PutIfAbsent(ctx context.Context, digest string, r io.Reader) (bool, error)
	// HashesFor returns the persisted or recomputed multi-hashes for a blob.
	HashesFor(ctx context.Context, digest string) (Hashes, error)
	// Delete removes the blob.
	Delete(ctx context.Context, digest string) error
	// List returns every digest in the store.
	List(ctx context.Context) ([]string, error)
}

// UploadRecord is a persisted in-progress multi-chunk upload session.
type UploadRecord struct {
	ID       string `json:"id"`
	Format   string `json:"format"`
	Repository string `json:"repository"`
	Digest   string `json:"digest,omitempty"`
	Bytes    int64  `json:"bytes"`
	Complete bool   `json:"complete"`
}

// IndexStore abstracts the structured metadata layer (SQLite reference
// implementation). It holds the mutable index: repositories, versions, and
// the descriptors pointing at immutable blobs.
type IndexStore interface {
	// Put upserts an artifact's metadata by (format, repository, version).
	Put(ctx context.Context, a Artifact) error
	// Get fetches (format, repository, version) metadata.
	Get(ctx context.Context, format, repository, version string) (Artifact, error)
	// Delete removes one (format, repository, version).
	Delete(ctx context.Context, format, repository, version string) error
	// ListVersions returns all versions for a repository, ordered.
	ListVersions(ctx context.Context, format, repository string) ([]string, error)
	// ListRepositoriesByFormat returns every repository that has ≥1 version.
	ListRepositoriesByFormat(ctx context.Context, format string) ([]string, error)
	// ListRepositories returns every repository across all formats.
	ListRepositories(ctx context.Context) ([]string, error)
	// ListPackages returns a compact summary for the catalog.
	ListPackages(ctx context.Context) ([]PackageSummary, error)
	// DeleteRepo removes an entire repository for a format.
	DeleteRepo(ctx context.Context, format, repository string) (int, error)
	// SaveUpload persists an upload session.
	SaveUpload(ctx context.Context, u UploadRecord) error
	// GetUpload returns a session by id.
	GetUpload(ctx context.Context, id string) (UploadRecord, error)
	// DeleteUpload removes a session.
	DeleteUpload(ctx context.Context, id string) error
	// ListUploads returns session ids.
	ListUploads(ctx context.Context) ([]string, error)
	// GetMeta returns a per-repository metadata blob (e.g. a resolved index).
	GetMeta(ctx context.Context, format, repository string) ([]byte, error)
	// SetMeta stores a per-repository metadata blob.
	SetMeta(ctx context.Context, format, repository string, data []byte) error
	// Close releases the underlying resources.
	Close() error
}

// Sentinels for mapping store errors onto protocol responses.
var (
	ErrArtifactUnknown = errors.New("artifact unknown")
	ErrBlobUnknown     = errors.New("blob unknown")
	ErrUploadUnknown   = errors.New("upload unknown")
)

// IsUnknown reports whether a store error is a plain "not found".
func IsUnknown(err error) bool {
	return errors.Is(err, ErrArtifactUnknown) ||
		errors.Is(err, ErrBlobUnknown) ||
		errors.Is(err, ErrUploadUnknown)
}
