package k8s

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/easylab-platform/easylab/internal/registry"
)

// ImageExists reports whether the OCI registry already has the image tag. It
// is best-effort: a missing repo/tag is a 404, and any other failure returns
// false so the caller rebuilds. The probe targets easylab's in-cluster v2
// endpoint (EASYLAB_ARTIFACT_URL when set, else the registry host).
func (c *Client) ImageExists(ctx context.Context, ref string) bool {
	_, repo, tag := registry.Split(ref)
	if repo == "" {
		return false
	}
	base := os.Getenv("EASYLAB_ARTIFACT_URL")
	if base == "" {
		base = "http://" + c.registryHost
	}
	u := strings.TrimSuffix(base, "/") + "/v2/" + repo + "/manifests/" + tag
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
	if err != nil {
		return false
	}
	if c.registryToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.registryToken)
	}
	req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}
