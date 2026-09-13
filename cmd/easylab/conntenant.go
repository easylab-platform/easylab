package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"connectrpc.com/connect"

	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"
	"github.com/easylab-platform/easyvcs/store"
)

// connTenant is the Connect form of tenant administration. It wraps the same
// store + agent provisioning used by the REST operator endpoint
// (tenant_admin.go), with the authorization rules:
//
//   - Create/Update: default-tenant operator only.
//   - Get/List/Members and all tenant-scoped reads: the tenant's own
//     credential, or the default-tenant operator.
type connTenant struct {
	s *server
}

func (c *connTenant) CreateTenant(ctx context.Context, req *connect.Request[easylabv1.CreateTenantRequest]) (*connect.Response[easylabv1.CreateTenantResponse], error) {
	if tenantFromContext(ctx) != 1 {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("tenant creation requires a default-tenant operator credential"))
	}
	res, err := c.s.provisionTenant(req.Msg.Slug, req.Msg.DisplayName, req.Msg.AdminUsername, req.Msg.AdminDisplayName)
	if err != nil {
		return nil, statusErr(err)
	}
	return connect.NewResponse(&easylabv1.CreateTenantResponse{
		Tenant:      &easylabv1.Tenant{Id: res["tenant"].(string), Slug: res["slug"].(string), DisplayName: req.Msg.DisplayName},
		Member:      &easylabv1.TenantMember{Username: res["username"].(string), Role: store.RoleOwner},
		Token:       res["token"].(string),
		AgentTenant: res["agent_tenant"].(bool),
	}), nil
}

func (c *connTenant) GetTenant(ctx context.Context, req *connect.Request[easylabv1.GetTenantRequest]) (*connect.Response[easylabv1.GetTenantResponse], error) {
	id, err := strconv.ParseInt(req.Msg.Id, 10, 64)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid tenant id"))
	}
	if !c.canSeeTenant(ctx, id) {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not your tenant"))
	}
	t, err := c.s.cs.GetTenant(id)
	if err != nil {
		return nil, statusErr(err)
	}
	return connect.NewResponse(&easylabv1.GetTenantResponse{Tenant: tenantMsg(t)}), nil
}

func (c *connTenant) ListTenants(ctx context.Context, req *connect.Request[easylabv1.ListTenantsRequest]) (*connect.Response[easylabv1.ListTenantsResponse], error) {
	all, err := c.s.cs.ListTenants()
	if err != nil {
		return nil, statusErr(err)
	}
	out := make([]*easylabv1.Tenant, 0, len(all))
	if tenantFromContext(ctx) == 1 {
		for _, t := range all {
			out = append(out, tenantMsg(t))
		}
	} else {
		// Non-operators see only their own tenant.
		for _, t := range all {
			if t.ID == tenantFromContext(ctx) {
				out = append(out, tenantMsg(t))
			}
		}
	}
	return connect.NewResponse(&easylabv1.ListTenantsResponse{Tenants: out}), nil
}

func (c *connTenant) UpdateTenant(ctx context.Context, req *connect.Request[easylabv1.UpdateTenantRequest]) (*connect.Response[easylabv1.UpdateTenantResponse], error) {
	if tenantFromContext(ctx) != 1 {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("tenant update requires a default-tenant operator credential"))
	}
	id, err := strconv.ParseInt(req.Msg.Id, 10, 64)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid tenant id"))
	}
	t, err := c.s.cs.UpdateTenant(id, req.Msg.DisplayName, req.Msg.Disabled)
	if err != nil {
		return nil, statusErr(err)
	}
	return connect.NewResponse(&easylabv1.UpdateTenantResponse{Tenant: tenantMsg(t)}), nil
}

func (c *connTenant) ListTenantMembers(ctx context.Context, req *connect.Request[easylabv1.ListTenantMembersRequest]) (*connect.Response[easylabv1.ListTenantMembersResponse], error) {
	id, err := strconv.ParseInt(req.Msg.TenantId, 10, 64)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid tenant id"))
	}
	if !c.canSeeTenant(ctx, id) {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not your tenant"))
	}
	users, err := c.s.cs.ListUsersByTenant(id)
	if err != nil {
		return nil, statusErr(err)
	}
	out := make([]*easylabv1.TenantMember, 0, len(users))
	for _, u := range users {
		role := c.s.cs.StrongestRoleOfUser(u.ID)
		if role == "" {
			role = store.RoleOwner // tenant creator holds no namespace row: owner by default
		}
		out = append(out, &easylabv1.TenantMember{Username: u.Username, Role: role})
	}
	return connect.NewResponse(&easylabv1.ListTenantMembersResponse{Members: out}), nil
}

func (c *connTenant) AddTenantMember(ctx context.Context, req *connect.Request[easylabv1.AddTenantMemberRequest]) (*connect.Response[easylabv1.AddTenantMemberResponse], error) {
	if tenantFromContext(ctx) != 1 {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("adding members requires a default-tenant operator credential"))
	}
	id, err := strconv.ParseInt(req.Msg.TenantId, 10, 64)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid tenant id"))
	}
	if req.Msg.Username == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("username required"))
	}
	role := req.Msg.Role
	if role == "" {
		role = store.RoleMember
	}
	u, err := c.s.cs.CreateUserTenant(id, req.Msg.Username, req.Msg.DisplayName)
	if err != nil {
		return nil, statusErr(err)
	}
	plaintext, err := generateToken()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	tok, err := c.s.cs.CreateToken(plaintext, u.ID, "write")
	if err != nil {
		return nil, statusErr(err)
	}
	if req.Msg.Namespace != "" {
		if err := c.s.cs.AddNamespaceMember(req.Msg.Namespace, u.ID, role); err != nil {
			return nil, statusErr(err)
		}
	}
	return connect.NewResponse(&easylabv1.AddTenantMemberResponse{
		Member: &easylabv1.TenantMember{Username: u.Username, Role: role},
		Token:  tok.Token,
	}), nil
}

// canSeeTenant: the default-tenant operator sees every tenant; a tenant
// credential sees only itself.
func (c *connTenant) canSeeTenant(ctx context.Context, id int64) bool {
	tid := tenantFromContext(ctx)
	return tid == 1 || tid == id
}

// statusErr maps store errors onto connect codes.
func statusErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, store.ErrUsernameTaken):
		return connect.NewError(connect.CodeAlreadyExists, err)
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}

func tenantMsg(t *store.Tenant) *easylabv1.Tenant {
	return &easylabv1.Tenant{
		Id:          strconv.FormatInt(t.ID, 10),
		Slug:        t.Slug,
		DisplayName: t.DisplayName,
		Disabled:    t.Disabled,
		CreatedAt:   t.Created.UTC().Format(time.RFC3339),
	}
}
