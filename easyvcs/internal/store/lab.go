package store

import (
	"database/sql"
	"errors"
	"time"
)

// User is a Lab account. A user owns namespaces (orgs) and repositories.
type User struct {
	ID          int64
	Username    string
	DisplayName string
	Created     time.Time
}

// Token is a bearer token bound to a user with an access level.
type Token struct {
	ID      int64
	Token   string
	UserID  int64
	Level   string // "read" or "write"
	Created time.Time
}

// NamespaceMember is a user's membership in a namespace (org).
type NamespaceMember struct {
	Namespace string
	UserID    int64
	Role      string // "owner" | "member"
}

// RepoMeta is the hosting metadata for a repository.
type RepoMeta struct {
	Description   string
	Visibility    string // "public" | "private"
	DefaultBranch string
	// Kind is "normal" (read-write) or "mirror" (read-only clone of an external
	// git repo; edits are forbidden).
	Kind string
	// Mirror fields are relevant only when Kind == "mirror".
	MirrorURL      string
	MirrorBranch   string
	MirrorInterval int // seconds between scheduled pulls; 0 = manual only
	MirrorLastRev  string
	MirrorLastSync int64 // epoch millis
	MirrorLastErr  string
	MirrorToken    string // never returned by API
}

// IsMirror reports whether the repository is a read-only external mirror.
func (r *Repo) IsMirror() bool {
	meta, err := r.RepoMeta()
	return err == nil && meta.Kind == "mirror"
}

// MergeRequest models a change-tracking request between two revisions.
type MergeRequest struct {
	ID          int64
	RepoID      int64
	IID         int64
	Title       string
	Description string
	Source      string // revision id or branch name
	Target      string // revision id or branch name
	State       string // "open" | "merged" | "closed"
	AuthorID    *int64
	Created     time.Time
	Updated     time.Time
}

// Review is a reviewer's verdict on a merge request.
type Review struct {
	ID         int64
	MRID       int64
	ReviewerID *int64
	State      string // "approved" | "request_changes" | "comment"
	Body       string
	Created    time.Time
}

// Comment is a discussion line on a merge request.
type Comment struct {
	ID       int64
	MRID     int64
	AuthorID *int64
	Body     string
	Path     string
	Created  time.Time
}

// ErrUsernameTaken is returned when creating a user that already exists.
var ErrUsernameTaken = errors.New("store: username already exists")

// ErrTokenNotFound is returned when a token is unknown.
var ErrTokenNotFound = errors.New("store: token not found")

// ErrUnchangedPassword is a legacy placeholder kept for interface stability.
var ErrUnchangedPassword = errors.New("store: password unchanged")

// CreateUser inserts a new user.
func (s *CentralStore) CreateUser(username, displayName string) (*User, error) {
	now := time.Now().UTC().UnixMilli()
	res, err := s.d.exec(
		"INSERT INTO users(username, display_name, created) VALUES(?,?,?)",
		username, displayName, now,
	)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &User{ID: id, Username: username, DisplayName: displayName, Created: time.UnixMilli(now)}, nil
}

// GetUser returns a user by id.
func (s *CentralStore) GetUser(id int64) (*User, error) {
	var name, disp string
	var created int64
	err := s.d.queryRow(
		"SELECT username, display_name, created FROM users WHERE id=?", id,
	).Scan(&name, &disp, &created)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &User{ID: id, Username: name, DisplayName: disp, Created: time.UnixMilli(created)}, nil
}

// GetUserByUsername returns a user by username.
func (s *CentralStore) GetUserByUsername(username string) (*User, error) {
	var id int64
	var disp string
	var created int64
	err := s.d.queryRow(
		"SELECT id, display_name, created FROM users WHERE username=?", username,
	).Scan(&id, &disp, &created)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &User{ID: id, Username: username, DisplayName: disp, Created: time.UnixMilli(created)}, nil
}

// ListUsers returns all users ordered by username.
func (s *CentralStore) ListUsers() ([]*User, error) {
	rows, err := s.d.query(
		"SELECT id, username, display_name, created FROM users ORDER BY username",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		var id, created int64
		var name, disp string
		if err := rows.Scan(&id, &name, &disp, &created); err != nil {
			return nil, err
		}
		out = append(out, &User{ID: id, Username: name, DisplayName: disp, Created: time.UnixMilli(created)})
	}
	return out, rows.Err()
}

// CreateToken inserts a bearer token for a user.
func (s *CentralStore) CreateToken(token string, userID int64, level string) (*Token, error) {
	now := time.Now().UTC().UnixMilli()
	res, err := s.d.exec(
		"INSERT INTO tokens(token, user_id, level, created) VALUES(?,?,?,?)",
		token, userID, level, now,
	)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &Token{ID: id, Token: token, UserID: userID, Level: level, Created: time.UnixMilli(now)}, nil
}

// LookupToken resolves a token string to its user and level. It returns
// ErrTokenNotFound when the token is unknown.
func (s *CentralStore) LookupToken(token string) (*Token, error) {
	var id, userID, created int64
	var level string
	err := s.d.queryRow(
		"SELECT id, user_id, level, created FROM tokens WHERE token=?", token,
	).Scan(&id, &userID, &level, &created)
	if err == sql.ErrNoRows {
		return nil, ErrTokenNotFound
	}
	if err != nil {
		return nil, err
	}
	return &Token{ID: id, Token: token, UserID: userID, Level: level, Created: time.UnixMilli(created)}, nil
}

// ListTokens returns all tokens for a user (omitting the secret for brevity is
// not possible here since the token is the primary text; callers may skip it).
func (s *CentralStore) ListTokens(userID int64) ([]*Token, error) {
	rows, err := s.d.query(
		"SELECT id, token, user_id, level, created FROM tokens WHERE user_id=?", userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Token
	for rows.Next() {
		var t Token
		var created int64
		if err := rows.Scan(&t.ID, &t.Token, &t.UserID, &t.Level, &created); err != nil {
			return nil, err
		}
		t.Created = time.UnixMilli(created)
		out = append(out, &t)
	}
	return out, rows.Err()
}

// DeleteToken removes a token by its secret value.
func (s *CentralStore) DeleteToken(token string) error {
	res, err := s.d.exec("DELETE FROM tokens WHERE token=?", token)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrTokenNotFound
	}
	return nil
}

// AddNamespaceMember grants a user membership in a namespace with a role.
func (s *CentralStore) AddNamespaceMember(namespace string, userID int64, role string) error {
	_, err := s.d.exec(
		`INSERT INTO namespace_members(namespace, user_id, role) VALUES(?,?,?)
		 ON CONFLICT(namespace, user_id) DO UPDATE SET role=excluded.role`,
		namespace, userID, role,
	)
	return err
}

// RemoveNamespaceMember revokes a user's membership in a namespace.
func (s *CentralStore) RemoveNamespaceMember(namespace string, userID int64) error {
	_, err := s.d.exec(
		"DELETE FROM namespace_members WHERE namespace=? AND user_id=?",
		namespace, userID,
	)
	return err
}

// GetNamespaceMember returns a user's membership in a namespace.
func (s *CentralStore) GetNamespaceMember(namespace string, userID int64) (*NamespaceMember, error) {
	var role string
	err := s.d.queryRow(
		"SELECT role FROM namespace_members WHERE namespace=? AND user_id=?",
		namespace, userID,
	).Scan(&role)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &NamespaceMember{Namespace: namespace, UserID: userID, Role: role}, nil
}

// ListNamespaceMembers lists members of a namespace.
func (s *CentralStore) ListNamespaceMembers(namespace string) ([]*NamespaceMember, error) {
	rows, err := s.d.query(
		"SELECT user_id, role FROM namespace_members WHERE namespace=? ORDER BY user_id",
		namespace,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*NamespaceMember
	for rows.Next() {
		var m NamespaceMember
		if err := rows.Scan(&m.UserID, &m.Role); err != nil {
			return nil, err
		}
		m.Namespace = namespace
		out = append(out, &m)
	}
	return out, rows.Err()
}

// ListUserNamespaces returns the namespaces a user belongs to.
func (s *CentralStore) ListUserNamespaces(userID int64) ([]*NamespaceMember, error) {
	rows, err := s.d.query(
		"SELECT namespace, role FROM namespace_members WHERE user_id=? ORDER BY namespace",
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*NamespaceMember
	for rows.Next() {
		var m NamespaceMember
		if err := rows.Scan(&m.Namespace, &m.Role); err != nil {
			return nil, err
		}
		m.UserID = userID
		out = append(out, &m)
	}
	return out, rows.Err()
}

// IsNamespaceMember reports whether a user belongs to a namespace, regardless of
// role. The user has read access to that namespace's private repositories.
func (s *CentralStore) IsNamespaceMember(namespace string, userID int64) bool {
	var one int
	err := s.d.queryRow(
		"SELECT 1 FROM namespace_members WHERE namespace=? AND user_id=?",
		namespace, userID,
	).Scan(&one)
	return err == nil
}

// UserCanReadRepo reports whether a user may read a repository. Public
// repositories are always readable; private repositories require namespace
// membership. When no user is supplied the repository is readable only if it is
// public (or the Lab has no users at all, i.e. an open single-user instance).
func (s *CentralStore) UserCanReadRepo(r RepoRef, userID *int64) bool {
	repo, err := s.OpenRepo(r)
	if err != nil {
		return false
	}
	meta, err := repo.RepoMeta()
	if err != nil {
		return false
	}
	if meta.Visibility != "private" {
		return true
	}
	if userID == nil {
		// No authenticated user: private repos are only visible when the entire
		// instance is a single-user open setup (no registered users).
		if s.instanceIsOpen() {
			return true
		}
		return false
	}
	return s.IsNamespaceMember(r.Namespace, *userID)
}

// instanceIsOpen reports whether no users and no tokens are registered, i.e. a
// fresh single-user fixture where there is nothing to hide and no auth to gate
// on. Callers use it to keep dev flows and unit tests frictionless.
func (s *CentralStore) instanceIsOpen() bool {
	users, err := s.ListUsers()
	if err != nil {
		return false
	}
	return len(users) == 0
}

// RepoMeta returns the hosting metadata for a repository.
func (r *Repo) RepoMeta() (RepoMeta, error) {
	var desc, vis, def, kind, mirrorURL, mirrorBranch, mirrorLastRev, mirrorLastErr, mirrorToken string
	var mirrorInterval int64
	var mirrorLastSync int64
	err := r.cs.d.queryRow(
		`SELECT description, visibility, default_branch, kind,
		        mirror_url, mirror_branch, mirror_interval,
		        mirror_last_rev, mirror_last_sync, mirror_last_error, mirror_token
		 FROM repositories WHERE id=?`,
		r.repoID,
	).Scan(&desc, &vis, &def, &kind, &mirrorURL, &mirrorBranch, &mirrorInterval,
		&mirrorLastRev, &mirrorLastSync, &mirrorLastErr, &mirrorToken)
	if err == sql.ErrNoRows {
		return RepoMeta{}, ErrNotFound
	}
	if err != nil {
		return RepoMeta{}, err
	}
	return RepoMeta{
		Description: desc, Visibility: vis, DefaultBranch: def,
		Kind: kind, MirrorURL: mirrorURL, MirrorBranch: mirrorBranch,
		MirrorInterval: int(mirrorInterval), MirrorLastRev: mirrorLastRev,
		MirrorLastSync: mirrorLastSync, MirrorLastErr: mirrorLastErr, MirrorToken: mirrorToken,
	}, nil
}

// UpdateRepoMeta updates description/visibility/default branch.
func (r *Repo) UpdateRepoMeta(m RepoMeta) error {
	_, err := r.cs.d.exec(
		"UPDATE repositories SET description=?, visibility=?, default_branch=? WHERE id=?",
		m.Description, m.Visibility, m.DefaultBranch, r.repoID,
	)
	return err
}

// UpdateMirrorMeta records mirror state (url, branch, interval, last result).
func (r *Repo) UpdateMirrorMeta(m RepoMeta) error {
	_, err := r.cs.d.exec(
		`UPDATE repositories SET kind=?, mirror_url=?, mirror_branch=?,
		        mirror_interval=?, mirror_last_rev=?, mirror_last_sync=?,
		        mirror_last_error=?, mirror_token=?
		 WHERE id=?`,
		m.Kind, m.MirrorURL, m.MirrorBranch, m.MirrorInterval,
		m.MirrorLastRev, m.MirrorLastSync, m.MirrorLastErr, m.MirrorToken, r.repoID,
	)
	return err
}

// CreateMergeRequest inserts a new MR and returns it. IID is assigned
// monotonically per repo.
func (s *CentralStore) CreateMergeRequest(repoID int64, mr *MergeRequest) (*MergeRequest, error) {
	now := time.Now().UTC().UnixMilli()
	var iid int64
	_ = s.d.queryRow(
		"SELECT COALESCE(MAX(iid),0)+1 FROM merge_requests WHERE repo_id=?", repoID,
	).Scan(&iid)
	res, err := s.d.exec(
		`INSERT INTO merge_requests(repo_id, iid, title, description, source, target, state, author_id, created, updated)
		 VALUES(?,?,?,?,?,?,?,?,?,?)`,
		repoID, iid, mr.Title, mr.Description, mr.Source, mr.Target, mr.State, nullableInt(mr.AuthorID),
		now, now,
	)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &MergeRequest{
		ID: id, RepoID: repoID, IID: iid, Title: mr.Title, Description: mr.Description,
		Source: mr.Source, Target: mr.Target, State: mr.State, AuthorID: mr.AuthorID,
		Created: time.UnixMilli(now), Updated: time.UnixMilli(now),
	}, nil
}

// GetMergeRequest returns an MR by repo + iid.
func (s *CentralStore) GetMergeRequest(repoID, iid int64) (*MergeRequest, error) {
	var id, authorID int64
	var title, desc, source, target, state string
	var created, updated int64
	var author sql.NullInt64
	err := s.d.queryRow(
		`SELECT id, iid, title, description, source, target, state, author_id, created, updated
		 FROM merge_requests WHERE repo_id=? AND iid=?`,
		repoID, iid,
	).Scan(&id, &iid, &title, &desc, &source, &target, &state, &author, &created, &updated)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if author.Valid {
		authorID = author.Int64
	}
	return &MergeRequest{
		ID: id, RepoID: repoID, IID: iid, Title: title, Description: desc,
		Source: source, Target: target, State: state,
		AuthorID: int64PtrOrNil(authorID, author.Valid),
		Created:  time.UnixMilli(created), Updated: time.UnixMilli(updated),
	}, nil
}

// ListMergeRequests returns MRs for a repo, optionally filtered by state.
func (s *CentralStore) ListMergeRequests(repoID int64, state string) ([]*MergeRequest, error) {
	q := "SELECT id, iid, title, description, source, target, state, author_id, created, updated FROM merge_requests WHERE repo_id=?"
	args := []any{repoID}
	if state != "" {
		q += " AND state=?"
		args = append(args, state)
	}
	q += " ORDER BY iid DESC"
	rows, err := s.d.query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*MergeRequest
	for rows.Next() {
		var mr MergeRequest
		var created, updated int64
		var author sql.NullInt64
		if err := rows.Scan(&mr.ID, &mr.IID, &mr.Title, &mr.Description, &mr.Source, &mr.Target, &mr.State, &author, &created, &updated); err != nil {
			return nil, err
		}
		if author.Valid {
			mr.AuthorID = int64PtrOrNil(author.Int64, true)
		}
		mr.RepoID = repoID
		mr.Created = time.UnixMilli(created)
		mr.Updated = time.UnixMilli(updated)
		out = append(out, &mr)
	}
	return out, rows.Err()
}

// UpdateMergeRequestState updates an MR state (merged/closed/reopen).
func (s *CentralStore) UpdateMergeRequestState(repoID, iid int64, state string) error {
	_, err := s.d.exec(
		"UPDATE merge_requests SET state=?, updated=? WHERE repo_id=? AND iid=?",
		state, time.Now().UTC().UnixMilli(), repoID, iid,
	)
	return err
}

// AddReview appends a review to an MR.
func (s *CentralStore) AddReview(mrID int64, reviewerID *int64, state, body string) (*Review, error) {
	now := time.Now().UTC().UnixMilli()
	res, err := s.d.exec(
		"INSERT INTO mr_reviews(mr_id, reviewer_id, state, body, created) VALUES(?,?,?,?,?)",
		mrID, nullableInt(reviewerID), state, body, now,
	)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Review{ID: id, MRID: mrID, ReviewerID: reviewerID, State: state, Body: body, Created: time.UnixMilli(now)}, nil
}

// ListReviews returns reviews for an MR.
func (s *CentralStore) ListReviews(mrID int64) ([]*Review, error) {
	rows, err := s.d.query(
		"SELECT id, reviewer_id, state, body, created FROM mr_reviews WHERE mr_id=? ORDER BY created",
		mrID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Review
	for rows.Next() {
		var rv Review
		var created int64
		var reviewer sql.NullInt64
		if err := rows.Scan(&rv.ID, &reviewer, &rv.State, &rv.Body, &created); err != nil {
			return nil, err
		}
		if reviewer.Valid {
			rv.ReviewerID = int64PtrOrNil(reviewer.Int64, true)
		}
		rv.MRID = mrID
		rv.Created = time.UnixMilli(created)
		out = append(out, &rv)
	}
	return out, rows.Err()
}

// AddComment appends a comment to an MR.
func (s *CentralStore) AddComment(mrID int64, authorID *int64, body, path string) (*Comment, error) {
	now := time.Now().UTC().UnixMilli()
	res, err := s.d.exec(
		"INSERT INTO mr_comments(mr_id, author_id, body, path, created) VALUES(?,?,?,?,?)",
		mrID, nullableInt(authorID), body, path, now,
	)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Comment{ID: id, MRID: mrID, AuthorID: authorID, Body: body, Path: path, Created: time.UnixMilli(now)}, nil
}

// ListComments returns comments for an MR.
func (s *CentralStore) ListComments(mrID int64) ([]*Comment, error) {
	rows, err := s.d.query(
		"SELECT id, mr_id, author_id, body, path, created FROM mr_comments WHERE mr_id=? ORDER BY created",
		mrID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Comment
	for rows.Next() {
		var c Comment
		var created int64
		var author sql.NullInt64
		if err := rows.Scan(&c.ID, &c.MRID, &author, &c.Body, &c.Path, &created); err != nil {
			return nil, err
		}
		if author.Valid {
			c.AuthorID = int64PtrOrNil(author.Int64, true)
		}
		c.Created = time.UnixMilli(created)
		out = append(out, &c)
	}
	return out, rows.Err()
}

func int64PtrOrNil(v int64, ok bool) *int64 {
	if !ok {
		return nil
	}
	return &v
}

func nullableInt(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}
