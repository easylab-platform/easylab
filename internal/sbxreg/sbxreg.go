// Package sbxreg is easylab's sandbox registry: the persistent association
// of every sandbox container with its org/repo/branch workspace (empty org =
// standalone sandbox), the derived image it runs, and the rev-coherence
// metadata (last synced rev + worker boot id at sync time). It lives in the
// same sqlite/postgres database as the easyvcs store (own GORM session,
// WAL + busy_timeout).
package sbxreg

import (
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// Sandbox is one registered sandbox container.
type Sandbox struct {
	Name         string `gorm:"primaryKey"` // container/service name
	Org          string // '' = standalone sandbox (never synced)
	Repo         string
	Branch       string
	BaseImage    string
	DerivedImage string
	Workspace    string // default /workspace ('/tmp' for nonroot bases)
	SyncedRev    string // rev of the workspace tree last pushed
	SyncedBootID string // worker boot id at sync time (restart detection)
	// Token is the bearer token easylab uses to authenticate to the worker.
	// Per-sandbox unique. Managed sandboxes get it injected at launch; external
	// sandboxes get it from the one-time enrollment claim (or operator-supplied).
	Token string
	// Mode is "managed" (easylab launched it) or "external" (registered by an
	// operator; no podman container easylab owns).
	Mode string
	// Addr is the worker base URL for external sandboxes (managed sandboxes
	// resolve their loopback address from the podman-published port instead).
	Addr string
	// OwnerID is the caller identity that claimed an external sandbox (audit).
	OwnerID     string
	CreatedAtMs int64 `gorm:"autoCreateTime:milli"`
	UpdatedAtMs int64 `gorm:"autoUpdateTime:milli"`
}

// Registry is the table handle.
type Registry struct{ db *gorm.DB }

// Open opens (creating if needed) the sandboxes table on the same database
// the caller picked for the easyvcs store. kind: "sqlite" (dsn = file path)
// or "postgres" (dsn = URL).
func Open(kind, dsn string) (*Registry, error) {
	var dial gorm.Dialector
	switch kind {
	case "postgres":
		dial = postgres.Open(dsn)
	default:
		dial = sqlite.Open(dsn + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	}
	db, err := gorm.Open(dial, &gorm.Config{})
	if err != nil {
		return nil, err
	}
	if err := db.AutoMigrate(&Sandbox{}); err != nil {
		return nil, err
	}
	return &Registry{db: db}, nil
}

// Upsert inserts or updates a registration (keeping sync metadata on update
// when the new row leaves them empty).
func (r *Registry) Upsert(s Sandbox) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		var old Sandbox
		err := tx.Where("name = ?", s.Name).First(&old).Error
		if err == nil {
			if s.SyncedRev == "" {
				s.SyncedRev = old.SyncedRev
			}
			if s.SyncedBootID == "" {
				s.SyncedBootID = old.SyncedBootID
			}
			if s.Token == "" {
				s.Token = old.Token
			}
			if s.Mode == "" {
				s.Mode = old.Mode
			}
			if s.Addr == "" {
				s.Addr = old.Addr
			}
			if s.OwnerID == "" {
				s.OwnerID = old.OwnerID
			}
			return tx.Model(&Sandbox{}).Where("name = ?", s.Name).Updates(map[string]interface{}{
				"org": s.Org, "repo": s.Repo, "branch": s.Branch,
				"base_image": s.BaseImage, "derived_image": s.DerivedImage,
				"workspace": s.Workspace, "synced_rev": s.SyncedRev,
				"synced_boot_id": s.SyncedBootID, "token": s.Token,
				"mode": s.Mode, "addr": s.Addr, "owner_id": s.OwnerID,
				"updated_at_ms": time.Now().UnixMilli(),
			}).Error
		}
		if err != gorm.ErrRecordNotFound {
			return err
		}
		return tx.Create(&s).Error
	})
}

// Get returns one registration.
func (r *Registry) Get(name string) (Sandbox, bool, error) {
	var s Sandbox
	err := r.db.Where("name = ?", name).First(&s).Error
	if err == gorm.ErrRecordNotFound {
		return Sandbox{}, false, nil
	}
	return s, err == nil, err
}

// List returns every registration.
func (r *Registry) List() ([]Sandbox, error) {
	var out []Sandbox
	err := r.db.Order("updated_at_ms DESC").Find(&out).Error
	return out, err
}

// MarkSynced records a successful workspace sync (rev + worker boot id).
func (r *Registry) MarkSynced(name, rev, bootID string) error {
	return r.db.Model(&Sandbox{}).Where("name = ?", name).Updates(map[string]interface{}{
		"synced_rev": rev, "synced_boot_id": bootID, "updated_at_ms": time.Now().UnixMilli(),
	}).Error
}

// Delete drops a registration.
func (r *Registry) Delete(name string) error {
	return r.db.Where("name = ?", name).Delete(&Sandbox{}).Error
}
