package main

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"connectrpc.com/connect"

	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"
	"github.com/easylab-platform/easyvcs/store"
)

// Merge requests (fork→upstream change requests) and repo collaborator
// authorization, as the RPC form of the removed /api/v1 merge_requests and
// namespace-member REST endpoints.

func mrView(s *server, m *store.MergeRequest) *easylabv1.MergeRequest {
	v := &easylabv1.MergeRequest{
		Iid: strconv.FormatInt(m.IID, 10), Title: m.Title, Description: m.Description,
		Source: m.Source, Target: m.Target, State: m.State,
		CreatedAt: m.Created.UTC().Format(time.RFC3339),
		UpdatedAt: m.Updated.UTC().Format(time.RFC3339),
	}
	if m.AuthorID != nil {
		if u, _ := s.cs.GetUser(*m.AuthorID); u != nil {
			v.Author = u.Username
		}
	}
	return v
}

func (c *connLab) ListMergeRequests(ctx context.Context, req *connect.Request[easylabv1.ListMergeRequestsRequest]) (*connect.Response[easylabv1.ListMergeRequestsResponse], error) {
	repo, err := c.s.openRepoAuthorized(ctx, req.Msg.Org, req.Msg.Repo, repoCanRead, "reading change requests")
	if err != nil {
		return nil, err
	}
	mrs, err := c.s.cs.ListMergeRequests(repo.RepoID(), req.Msg.State)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*easylabv1.MergeRequest, 0, len(mrs))
	for _, m := range mrs {
		out = append(out, mrView(c.s, m))
	}
	return connect.NewResponse(&easylabv1.ListMergeRequestsResponse{MergeRequests: out}), nil
}

func (c *connLab) GetMergeRequest(ctx context.Context, req *connect.Request[easylabv1.GetMergeRequestRequest]) (*connect.Response[easylabv1.GetMergeRequestResponse], error) {
	repo, err := c.s.openRepoAuthorized(ctx, req.Msg.Org, req.Msg.Repo, repoCanRead, "reading a change request")
	if err != nil {
		return nil, err
	}
	iid, err := strconv.ParseInt(req.Msg.Iid, 10, 64)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid iid"))
	}
	mr, err := c.s.cs.GetMergeRequest(repo.RepoID(), iid)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewResponse(&easylabv1.GetMergeRequestResponse{MergeRequest: mrView(c.s, mr)}), nil
}

func (c *connLab) CreateMergeRequest(ctx context.Context, req *connect.Request[easylabv1.CreateMergeRequestRequest]) (*connect.Response[easylabv1.CreateMergeRequestResponse], error) {
	// Opening a change request is a proposal: developer+ (authenticated).
	if err := c.s.requireRepoAction(ctx, req.Msg.Org, req.Msg.Repo, repoCanPropose, "opening a change request"); err != nil {
		return nil, err
	}
	if err := requireAuthenticated(ctx, "opening a change request"); err != nil {
		return nil, err
	}
	repo, err := c.s.openRepoAuthorized(ctx, req.Msg.Org, req.Msg.Repo, repoCanRead, "reading a repository")
	if err != nil {
		return nil, err
	}
	if req.Msg.Title == "" || req.Msg.Source == "" || req.Msg.Target == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("title, source, target required"))
	}
	authorID := userIDOf(ctx)
	mr, err := c.s.cs.CreateMergeRequest(repo.RepoID(), &store.MergeRequest{
		Title: req.Msg.Title, Description: req.Msg.Description,
		Source: req.Msg.Source, Target: req.Msg.Target, State: "open", AuthorID: &authorID,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&easylabv1.CreateMergeRequestResponse{Ok: true, MergeRequest: mrView(c.s, mr)}), nil
}

func (c *connLab) UpdateMergeRequest(ctx context.Context, req *connect.Request[easylabv1.UpdateMergeRequestRequest]) (*connect.Response[easylabv1.UpdateMergeRequestResponse], error) {
	// Closing/reopening is a maintainer action on the target repo.
	if err := c.s.requireRepoAction(ctx, req.Msg.Org, req.Msg.Repo, repoCanMerge, "updating a change request"); err != nil {
		return nil, err
	}
	repo, err := c.s.openRepoAuthorized(ctx, req.Msg.Org, req.Msg.Repo, repoCanRead, "reading a repository")
	if err != nil {
		return nil, err
	}
	iid, err := strconv.ParseInt(req.Msg.Iid, 10, 64)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid iid"))
	}
	switch req.Msg.State {
	case "close", "closed":
		req.Msg.State = "closed"
	case "reopen", "open":
		req.Msg.State = "open"
	case "merge", "merged":
		req.Msg.State = "merged"
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid state %q", req.Msg.State))
	}
	if err := c.s.cs.UpdateMergeRequestState(repo.RepoID(), iid, req.Msg.State); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewResponse(&easylabv1.UpdateMergeRequestResponse{Ok: true}), nil
}

func (c *connLab) MergeMergeRequest(ctx context.Context, req *connect.Request[easylabv1.MergeMergeRequestRequest]) (*connect.Response[easylabv1.MergeMergeRequestResponse], error) {
	if err := c.s.requireRepoAction(ctx, req.Msg.Org, req.Msg.Repo, repoCanMerge, "merging a change request"); err != nil {
		return nil, err
	}
	repo, err := c.s.openRepoAuthorized(ctx, req.Msg.Org, req.Msg.Repo, repoCanRead, "reading a repository")
	if err != nil {
		return nil, err
	}
	iid, err := strconv.ParseInt(req.Msg.Iid, 10, 64)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid iid"))
	}
	mr, err := c.s.cs.GetMergeRequest(repo.RepoID(), iid)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	revID, snap, conflicts, err := c.s.mergeMergeRequest(repo, mr)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := c.s.cs.UpdateMergeRequestState(repo.RepoID(), iid, "merged"); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&easylabv1.MergeMergeRequestResponse{Ok: true, RevisionId: revID, Snapshot: snap, Conflicts: int32(conflicts)}), nil
}

// ---- reviews / comments ----

func (c *connLab) ListReviews(ctx context.Context, req *connect.Request[easylabv1.ListReviewsRequest]) (*connect.Response[easylabv1.ListReviewsResponse], error) {
	repo, err := c.s.openRepoAuthorized(ctx, req.Msg.Org, req.Msg.Repo, repoCanRead, "reading reviews")
	if err != nil {
		return nil, err
	}
	iid, err := strconv.ParseInt(req.Msg.Iid, 10, 64)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid iid"))
	}
	mr, err := c.s.cs.GetMergeRequest(repo.RepoID(), iid)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	reviews, err := c.s.cs.ListReviews(mr.ID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*easylabv1.MergeReview, 0, len(reviews))
	for _, r := range reviews {
		name := ""
		if r.ReviewerID != nil {
			if u, _ := c.s.cs.GetUser(*r.ReviewerID); u != nil {
				name = u.Username
			}
		}
		out = append(out, &easylabv1.MergeReview{Reviewer: name, State: r.State, Body: r.Body, CreatedAt: r.Created.UTC().Format(time.RFC3339)})
	}
	return connect.NewResponse(&easylabv1.ListReviewsResponse{Reviews: out}), nil
}

func (c *connLab) AddReview(ctx context.Context, req *connect.Request[easylabv1.AddReviewRequest]) (*connect.Response[easylabv1.AddReviewResponse], error) {
	// Reviewing (comment/approve) is a proposal-level action: developer+.
	if err := c.s.requireRepoAction(ctx, req.Msg.Org, req.Msg.Repo, repoCanPropose, "reviewing a change request"); err != nil {
		return nil, err
	}
	if err := requireAuthenticated(ctx, "reviewing a change request"); err != nil {
		return nil, err
	}
	repo, err := c.s.openRepoAuthorized(ctx, req.Msg.Org, req.Msg.Repo, repoCanRead, "reading a repository")
	if err != nil {
		return nil, err
	}
	iid, err := strconv.ParseInt(req.Msg.Iid, 10, 64)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid iid"))
	}
	mr, err := c.s.cs.GetMergeRequest(repo.RepoID(), iid)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	uid := userIDOf(ctx)
	if _, err := c.s.cs.AddReview(mr.ID, &uid, req.Msg.State, req.Msg.Body); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&easylabv1.AddReviewResponse{Ok: true}), nil
}

func (c *connLab) ListComments(ctx context.Context, req *connect.Request[easylabv1.ListCommentsRequest]) (*connect.Response[easylabv1.ListCommentsResponse], error) {
	repo, err := c.s.openRepoAuthorized(ctx, req.Msg.Org, req.Msg.Repo, repoCanRead, "reading comments")
	if err != nil {
		return nil, err
	}
	iid, err := strconv.ParseInt(req.Msg.Iid, 10, 64)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid iid"))
	}
	mr, err := c.s.cs.GetMergeRequest(repo.RepoID(), iid)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	comments, err := c.s.cs.ListComments(mr.ID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*easylabv1.MergeComment, 0, len(comments))
	for _, cm := range comments {
		name := ""
		if cm.AuthorID != nil {
			if u, _ := c.s.cs.GetUser(*cm.AuthorID); u != nil {
				name = u.Username
			}
		}
		out = append(out, &easylabv1.MergeComment{Author: name, Body: cm.Body, Path: cm.Path, CreatedAt: cm.Created.UTC().Format(time.RFC3339)})
	}
	return connect.NewResponse(&easylabv1.ListCommentsResponse{Comments: out}), nil
}

func (c *connLab) AddComment(ctx context.Context, req *connect.Request[easylabv1.AddCommentRequest]) (*connect.Response[easylabv1.AddCommentResponse], error) {
	if err := c.s.requireRepoAction(ctx, req.Msg.Org, req.Msg.Repo, repoCanPropose, "commenting on a change request"); err != nil {
		return nil, err
	}
	if err := requireAuthenticated(ctx, "commenting on a change request"); err != nil {
		return nil, err
	}
	repo, err := c.s.openRepoAuthorized(ctx, req.Msg.Org, req.Msg.Repo, repoCanRead, "reading a repository")
	if err != nil {
		return nil, err
	}
	iid, err := strconv.ParseInt(req.Msg.Iid, 10, 64)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid iid"))
	}
	mr, err := c.s.cs.GetMergeRequest(repo.RepoID(), iid)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	uid := userIDOf(ctx)
	if _, err := c.s.cs.AddComment(mr.ID, &uid, req.Msg.Body, req.Msg.Path); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&easylabv1.AddCommentResponse{Ok: true}), nil
}

// ---- repo members (collaborator roles) ----

func (c *connLab) ListRepoMembers(ctx context.Context, req *connect.Request[easylabv1.ListRepoMembersRequest]) (*connect.Response[easylabv1.ListRepoMembersResponse], error) {
	repo, err := c.s.openRepoAuthorized(ctx, req.Msg.Org, req.Msg.Repo, repoCanRead, "reading collaborators")
	if err != nil {
		return nil, err
	}
	out := []*easylabv1.RepoMember{}
	if owner, oerr := c.s.cs.GetUser(repo.OwnerUserID); oerr == nil {
		out = append(out, &easylabv1.RepoMember{Username: owner.Username, Role: string(store.RoleOwner)})
	}
	members, err := c.s.cs.ListRepoMembers(repo.RepoID())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	for _, m := range members {
		entry := &easylabv1.RepoMember{Role: m.Role}
		if u, _ := c.s.cs.GetUser(m.UserID); u != nil {
			entry.Username = u.Username
		}
		out = append(out, entry)
	}
	return connect.NewResponse(&easylabv1.ListRepoMembersResponse{Members: out}), nil
}

func (c *connLab) SetRepoMember(ctx context.Context, req *connect.Request[easylabv1.SetRepoMemberRequest]) (*connect.Response[easylabv1.SetRepoMemberResponse], error) {
	// Managing collaborators is owner-only.
	if err := c.s.requireRepoAction(ctx, req.Msg.Org, req.Msg.Repo, repoCanPush, "managing collaborators"); err != nil {
		return nil, err
	}
	repo, err := c.s.openRepoAuthorized(ctx, req.Msg.Org, req.Msg.Repo, repoCanRead, "reading a repository")
	if err != nil {
		return nil, err
	}
	target, err := c.s.cs.GetUserByUsername(req.Msg.Username)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("user %q not found", req.Msg.Username))
	}
	role := req.Msg.Role
	if role == "" {
		role = store.RoleDeveloper
	}
	by := userIDOf(ctx)
	if err := c.s.cs.SetRepoMember(repo.RepoID(), target.ID, role, &by); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&easylabv1.SetRepoMemberResponse{Ok: true}), nil
}

func (c *connLab) RemoveRepoMember(ctx context.Context, req *connect.Request[easylabv1.RemoveRepoMemberRequest]) (*connect.Response[easylabv1.RemoveRepoMemberResponse], error) {
	if err := c.s.requireRepoAction(ctx, req.Msg.Org, req.Msg.Repo, repoCanPush, "managing collaborators"); err != nil {
		return nil, err
	}
	repo, err := c.s.openRepoAuthorized(ctx, req.Msg.Org, req.Msg.Repo, repoCanRead, "reading a repository")
	if err != nil {
		return nil, err
	}
	target, err := c.s.cs.GetUserByUsername(req.Msg.Username)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("user %q not found", req.Msg.Username))
	}
	if err := c.s.cs.RemoveRepoMember(repo.RepoID(), target.ID); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&easylabv1.RemoveRepoMemberResponse{Ok: true}), nil
}
