package main

import (
	"context"
	"sort"
	"strconv"
	"strings"

	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"

	"connectrpc.com/connect"
	"github.com/easylab-platform/artifact/core"
)

// connRegistry implements easylabv1connect.RegistryServiceHandler over the
// artifactkit index store.
type connRegistry struct {
	s *server
}

func (c *connRegistry) ListPackageTypes(ctx context.Context, req *connect.Request[easylabv1.ListPackageTypesRequest]) (*connect.Response[easylabv1.ListPackageTypesResponse], error) {
	idx := c.s.registry.Meta
	// Group by the index's real format column. Splitting repository keys at
	// "/" used to surface OCI namespaces (library/, team/) as fake package
	// protocols and hide real ones (npm) entirely.
	packages, _ := idx.ListPackages(ctx)
	counts := map[string]int32{}
	for _, p := range packages {
		counts[p.Format]++
	}
	out := make([]*easylabv1.PackageTypeEntry, 0, len(counts))
	for format, n := range counts {
		out = append(out, &easylabv1.PackageTypeEntry{Type: format, Packages: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return connect.NewResponse(&easylabv1.ListPackageTypesResponse{Packages: out}), nil
}

func (c *connRegistry) ListPackages(ctx context.Context, req *connect.Request[easylabv1.ListPackagesRequest]) (*connect.Response[easylabv1.ListPackagesResponse], error) {
	idx := c.s.registry.Meta
	// The index summary carries Format and Repository SEPARATELY; the raw
	// ListRepositories only returns repository names (no format), so splitting
	// on "/" would mis-attribute scoped names (@scope/name) and OCI paths.
	summaries, err := idx.ListPackages(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	type key struct{ format, repository string }
	seen := map[key]bool{}
	var order []key
	for _, s := range summaries {
		k := key{s.Format, s.Repository}
		if !seen[k] {
			seen[k] = true
			order = append(order, k)
		}
	}
	uid := userIDOf(ctx)
	var out []*easylabv1.PackageInfo
	for _, k := range order {
		if req.Msg.Type != "" && k.format != req.Msg.Type {
			continue
		}
		if !c.s.cs.CanRead(ctx, k.format, k.repository, uid) {
			continue
		}
		versions, verr := idx.ListVersions(ctx, k.format, k.repository)
		if verr != nil {
			continue
		}
		pi := &easylabv1.PackageInfo{Type: k.format, Name: k.repository, Visibility: "public"}
		if own, oerr := c.s.cs.GetPackageOwner(k.format, k.repository); oerr == nil {
			pi.Visibility = own.Visibility
			if own.OwnerUserID != 0 {
				pi.Owner = strconv.FormatInt(own.OwnerUserID, 10)
			}
		}
		for _, v := range versions {
			pi.Versions = append(pi.Versions, &easylabv1.PackageVersion{Version: v})
		}
		out = append(out, pi)
	}
	return connect.NewResponse(&easylabv1.ListPackagesResponse{Packages: out}), nil
}

// SetPackageVisibility flips a package's visibility (maintainer+ on its scope).
func (c *connRegistry) SetPackageVisibility(ctx context.Context, req *connect.Request[easylabv1.SetPackageVisibilityRequest]) (*connect.Response[easylabv1.SetPackageVisibilityResponse], error) {
	if err := requireAuthenticated(ctx, "changing package visibility"); err != nil {
		return nil, err
	}
	if err := c.s.cs.SetPackageVisibilityAuthorized(req.Msg.Type, req.Msg.Name, userIDOf(ctx), req.Msg.Visibility); err != nil {
		return nil, connect.NewError(connect.CodePermissionDenied, err)
	}
	return connect.NewResponse(&easylabv1.SetPackageVisibilityResponse{Ok: true}), nil
}

func (c *connRegistry) PackageVersions(ctx context.Context, req *connect.Request[easylabv1.PackageVersionsRequest]) (*connect.Response[easylabv1.PackageVersionsResponse], error) {
	idx := c.s.registry.Meta
	versions, err := idx.ListVersions(ctx, req.Msg.Type, req.Msg.Name)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	var out []*easylabv1.PackageVersion
	for _, v := range versions {
		out = append(out, &easylabv1.PackageVersion{Version: v})
	}
	return connect.NewResponse(&easylabv1.PackageVersionsResponse{Versions: out}), nil
}

func (c *connRegistry) DeletePackage(ctx context.Context, req *connect.Request[easylabv1.DeletePackageRequest]) (*connect.Response[easylabv1.DeletePackageResponse], error) {
	_, err := c.s.registry.Meta.DeleteRepo(ctx, req.Msg.Type, req.Msg.Name)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&easylabv1.DeletePackageResponse{Ok: true}), nil
}

func (c *connRegistry) DeletePackageVersion(ctx context.Context, req *connect.Request[easylabv1.DeletePackageVersionRequest]) (*connect.Response[easylabv1.DeletePackageVersionResponse], error) {
	if err := c.s.registry.Meta.Delete(ctx, req.Msg.Type, req.Msg.Name, req.Msg.Version); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&easylabv1.DeletePackageVersionResponse{Ok: true}), nil
}

func (c *connRegistry) ListPublishSpecs(ctx context.Context, req *connect.Request[easylabv1.ListPublishSpecsRequest]) (*connect.Response[easylabv1.ListPublishSpecsResponse], error) {
	specs := make([]*easylabv1.PublishSpec, 0)
	for _, name := range artifactkit.Registered() {
		specs = append(specs, &easylabv1.PublishSpec{Protocol: name})
	}
	return connect.NewResponse(&easylabv1.ListPublishSpecsResponse{Specs: specs}), nil
}

// OCICatalog returns the OCI repositories in the internal registry.
func (c *connRegistry) OCICatalog(ctx context.Context, req *connect.Request[easylabv1.OCICatalogRequest]) (*connect.Response[easylabv1.OCICatalogResponse], error) {
	repos, err := c.s.registry.Meta.ListRepositories(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]string, 0, len(repos))
	for _, r := range repos {
		if !strings.HasPrefix(r, "oci/") {
			continue
		}
		out = append(out, strings.TrimPrefix(r, "oci/"))
	}
	return connect.NewResponse(&easylabv1.OCICatalogResponse{Repositories: out}), nil
}

// splitFormat extracts the format portion from a artifactkit repo key. Repos are
// stored as "{format}/{repository}"; the generic/Lab release protocol uses a
// single segment "ns:repo" with no format prefix, so it defaults to generic.
func splitFormat(repo string) string {
	if i := strings.Index(repo, "/"); i > 0 {
		return repo[:i]
	}
	return "generic"
}

// splitRepo returns (format, repository) from a artifactkit repo key.
func splitRepo(repo string) (string, string) {
	if i := strings.Index(repo, "/"); i > 0 {
		return repo[:i], repo[i+1:]
	}
	return "generic", repo
}

var _ = strings.TrimSpace
