package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/abcp-sdk/agent-sdk-go/agent/v1"
	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"
	"github.com/easylab-platform/easyvcs/store"
)

// connUser implements easylab.v1.UserService. User administration is the
// admin surface: create/update/delete and token minting require the static
// admin credential; reads require an authenticated caller.
type connUser struct {
	s *server
}

func userMsg(u *store.User) *easylabv1.User {
	return &easylabv1.User{
		Id:          strconv.FormatInt(u.ID, 10),
		Username:    u.Username,
		DisplayName: u.DisplayName,
		Disabled:    u.Disabled,
		CreatedAt:   u.Created.UTC().Format(time.RFC3339),
		AgentTenant: u.AgentTenant,
	}
}

func (c *connUser) CreateUser(ctx context.Context, req *connect.Request[easylabv1.CreateUserRequest]) (*connect.Response[easylabv1.CreateUserResponse], error) {
	if err := requireAdmin(ctx, "creating a user"); err != nil {
		return nil, err
	}
	u, err := c.s.cs.CreateUser(req.Msg.Username, req.Msg.DisplayName)
	if err != nil {
		return nil, statusErr(err)
	}
	plaintext, err := generateToken()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if _, err := c.s.cs.CreateToken(plaintext, u.ID, "write"); err != nil {
		return nil, statusErr(err)
	}
	if req.Msg.ProvisionAgentTenant && c.s.agentAdmin() != nil {
		actx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		res, aerr := c.s.agentAdmin().CreateTenant(actx, connect.NewRequest(&agentv1.CreateTenantRequest{Id: u.Username, Name: u.DisplayName}))
		if aerr != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("agent tenant create: %w", aerr))
		}
		if err := c.s.cs.SetAgentBinding(u.ID, u.Username, res.Msg.GetToken()); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		u.AgentTenant = u.Username
	}
	return connect.NewResponse(&easylabv1.CreateUserResponse{User: userMsg(u), Token: plaintext}), nil
}

func (c *connUser) GetUser(ctx context.Context, req *connect.Request[easylabv1.GetUserRequest]) (*connect.Response[easylabv1.GetUserResponse], error) {
	if err := requireAuthenticated(ctx, "reading a user"); err != nil {
		return nil, err
	}
	u, err := c.resolveUser(req.Msg.Id)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&easylabv1.GetUserResponse{User: userMsg(u)}), nil
}

func (c *connUser) ListUsers(ctx context.Context, req *connect.Request[easylabv1.ListUsersRequest]) (*connect.Response[easylabv1.ListUsersResponse], error) {
	if err := requireAuthenticated(ctx, "listing users"); err != nil {
		return nil, err
	}
	users, err := c.s.cs.ListUsers()
	if err != nil {
		return nil, statusErr(err)
	}
	out := make([]*easylabv1.User, 0, len(users))
	for _, u := range users {
		out = append(out, userMsg(u))
	}
	return connect.NewResponse(&easylabv1.ListUsersResponse{Users: out}), nil
}

func (c *connUser) UpdateUser(ctx context.Context, req *connect.Request[easylabv1.UpdateUserRequest]) (*connect.Response[easylabv1.UpdateUserResponse], error) {
	if err := requireAdmin(ctx, "updating a user"); err != nil {
		return nil, err
	}
	id, err := parseUserID(req.Msg.Id)
	if err != nil {
		return nil, err
	}
	u, err := c.s.cs.UpdateUser(id, req.Msg.DisplayName, req.Msg.Disabled)
	if err != nil {
		return nil, statusErr(err)
	}
	// Propagate disabled to the agent tenant (best effort).
	if req.Msg.Disabled != nil && *req.Msg.Disabled && c.s.agentAdmin() != nil {
		if at, _, berr := c.s.cs.AgentBinding(id); berr == nil && at != "" {
			actx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _ = c.s.agentAdmin().UpdateTenant(actx, connect.NewRequest(&agentv1.UpdateTenantRequest{Id: at, Disabled: req.Msg.Disabled}))
		}
	}
	return connect.NewResponse(&easylabv1.UpdateUserResponse{User: userMsg(u)}), nil
}

func (c *connUser) DeleteUser(ctx context.Context, req *connect.Request[easylabv1.DeleteUserRequest]) (*connect.Response[easylabv1.DeleteUserResponse], error) {
	if err := requireAdmin(ctx, "deleting a user"); err != nil {
		return nil, err
	}
	id, err := parseUserID(req.Msg.Id)
	if err != nil {
		return nil, err
	}
	agentDeleted := false
	if at, _, berr := c.s.cs.AgentBinding(id); berr == nil && at != "" && c.s.agentAdmin() != nil {
		actx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if _, aerr := c.s.agentAdmin().DeleteTenant(actx, connect.NewRequest(&agentv1.DeleteTenantRequest{Id: at})); aerr != nil {
			return connect.NewResponse(&easylabv1.DeleteUserResponse{Ok: false, AgentDeleted: false, Error: aerr.Error()}), nil
		}
		agentDeleted = true
	}
	if err := c.s.cs.DeleteUser(id); err != nil {
		return nil, statusErr(err)
	}
	return connect.NewResponse(&easylabv1.DeleteUserResponse{Ok: true, AgentDeleted: agentDeleted}), nil
}

func (c *connUser) ListUserTokens(ctx context.Context, req *connect.Request[easylabv1.ListUserTokensRequest]) (*connect.Response[easylabv1.ListUserTokensResponse], error) {
	if err := requireAuthenticated(ctx, "listing tokens"); err != nil {
		return nil, err
	}
	id, err := parseUserID(req.Msg.UserId)
	if err != nil {
		return nil, err
	}
	toks, err := c.s.cs.ListTokens(id)
	if err != nil {
		return nil, statusErr(err)
	}
	out := make([]*easylabv1.UserToken, 0, len(toks))
	for _, t := range toks {
		out = append(out, &easylabv1.UserToken{
			Id:        strconv.FormatInt(t.ID, 10),
			CreatedAt: t.Created.UTC().Format(time.RFC3339),
		})
	}
	return connect.NewResponse(&easylabv1.ListUserTokensResponse{Tokens: out}), nil
}

func (c *connUser) CreateUserToken(ctx context.Context, req *connect.Request[easylabv1.CreateUserTokenRequest]) (*connect.Response[easylabv1.CreateUserTokenResponse], error) {
	if err := requireAdmin(ctx, "minting a token"); err != nil {
		return nil, err
	}
	id, err := parseUserID(req.Msg.UserId)
	if err != nil {
		return nil, err
	}
	plaintext := req.Msg.Token
	if plaintext == "" {
		plaintext, err = generateToken()
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}
	tok, err := c.s.cs.CreateToken(plaintext, id, "write")
	if err != nil {
		return nil, statusErr(err)
	}
	return connect.NewResponse(&easylabv1.CreateUserTokenResponse{Token: tok.Token}), nil
}

func (c *connUser) DeleteUserToken(ctx context.Context, req *connect.Request[easylabv1.DeleteUserTokenRequest]) (*connect.Response[easylabv1.DeleteUserTokenResponse], error) {
	if err := requireAdmin(ctx, "revoking a token"); err != nil {
		return nil, err
	}
	if err := c.s.cs.DeleteToken(req.Msg.Token); err != nil {
		return nil, statusErr(err)
	}
	return connect.NewResponse(&easylabv1.DeleteUserTokenResponse{Ok: true}), nil
}

// resolveUser accepts a numeric id or a username.
func (c *connUser) resolveUser(idOrName string) (*store.User, error) {
	if id, err := strconv.ParseInt(idOrName, 10, 64); err == nil {
		if u, err := c.s.cs.GetUser(id); err == nil {
			return u, nil
		}
	}
	u, err := c.s.cs.GetUserByUsername(idOrName)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("user %q not found", idOrName))
	}
	return u, nil
}

// parseUserID parses a numeric user id.
func parseUserID(id string) (int64, error) {
	v, err := strconv.ParseInt(strings.TrimSpace(id), 10, 64)
	if err != nil {
		return 0, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid user id %q", id))
	}
	return v, nil
}

// statusErr maps store errors onto connect codes.
func statusErr(err error) error {
	switch {
	case err == nil:
		return nil
	case err == store.ErrNotFound:
		return connect.NewError(connect.CodeNotFound, err)
	case err == store.ErrUsernameTaken:
		return connect.NewError(connect.CodeAlreadyExists, err)
	case strings.Contains(err.Error(), "UNIQUE constraint failed"):
		return connect.NewError(connect.CodeAlreadyExists, err)
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}
