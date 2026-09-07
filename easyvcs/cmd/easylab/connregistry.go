package main

import (
	"context"
	"strings"

	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"

	"connectrpc.com/connect"
	"github.com/pkr/pkrkit"
)

// connRegistry implements easylabv1connect.RegistryServiceHandler over the
// pkrkit index store.
type connRegistry struct {
	s *server
}

func (c *connRegistry) ListPackageTypes(ctx context.Context, req *connect.Request[easylabv1.ListPackageTypesRequest]) (*connect.Response[easylabv1.ListPackageTypesResponse], error) {
	idx := c.s.registry.Meta
	repos, err := idx.ListRepositories(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// Count packages per format from the list packages summary.
	packages, _ := idx.ListPackages(ctx)
	counts := map[string]int32{}
	for _, p := range packages {
		counts[p.Format]++
	}
	var out []*easylabv1.PackageTypeEntry
	seen := map[string]bool{}
	for _, r := range repos {
		format := splitFormat(r)
		if !seen[format] {
			seen[format] = true
			out = append(out, &easylabv1.PackageTypeEntry{
				Type:     format,
				Packages: counts[format],
			})
		}
	}
	return connect.NewResponse(&easylabv1.ListPackageTypesResponse{Packages: out}), nil
}

func (c *connRegistry) ListPackages(ctx context.Context, req *connect.Request[easylabv1.ListPackagesRequest]) (*connect.Response[easylabv1.ListPackagesResponse], error) {
	idx := c.s.registry.Meta
	repos, err := idx.ListRepositories(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	var out []*easylabv1.PackageInfo
	for _, r := range repos {
		format, name := splitRepo(r)
		if req.Msg.Type != "" && format != req.Msg.Type {
			continue
		}
		versions, verr := idx.ListVersions(ctx, format, name)
		if verr != nil {
			continue
		}
		private := &easylabv1.PackageInfo{Type: format, Name: name}
		for _, v := range versions {
			private.Versions = append(private.Versions, &easylabv1.PackageVersion{Version: v})
		}
		out = append(out, private)
	}
	return connect.NewResponse(&easylabv1.ListPackagesResponse{Packages: out}), nil
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
	for _, name := range pkrkit.Registered() {
		specs = append(specs, &easylabv1.PublishSpec{Protocol: name})
	}
	return connect.NewResponse(&easylabv1.ListPublishSpecsResponse{Specs: specs}), nil
}

// splitFormat extracts the format portion from a pkrkit repo key. Repos are
// stored as "{format}/{repository}"; the generic/Lab release protocol uses a
// single segment "ns:repo" with no format prefix, so it defaults to generic.
func splitFormat(repo string) string {
	if i := strings.Index(repo, "/"); i > 0 {
		return repo[:i]
	}
	return "generic"
}

// splitRepo returns (format, repository) from a pkrkit repo key.
func splitRepo(repo string) (string, string) {
	if i := strings.Index(repo, "/"); i > 0 {
		return repo[:i], repo[i+1:]
	}
	return "generic", repo
}

var _ = strings.TrimSpace
