package webapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// upstream is a thin HTTP client into easylab's own /api/v1 and /v2 surface.
// The aggregate layer uses it to fetch real service list, OCI catalog and
// release data instead of returning placeholders, keeping all EasyVCS
// semantics (repository/revision/ref, services, images, releases) in one place.
type upstream struct {
	self string // e.g. http://127.0.0.1:18160
	hc   *http.Client
}

func newUpstream(self string) *upstream {
	if self == "" {
		self = "http://127.0.0.1:18160"
	}
	return &upstream{self: strings.TrimSuffix(self, "/"), hc: &http.Client{Timeout: 20 * time.Second}}
}

func (u *upstream) get(ctx context.Context, path string, query url.Values) (int, []byte, error) {
	raw := u.self + path
	if len(query) > 0 {
		raw += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := u.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

func (u *upstream) post(ctx context.Context, path string, body any) (int, []byte, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.self+path, strings.NewReader(string(b)))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := u.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}

// services fetches /api/v1/ops/services and maps onto the UI container shape.
func (u *upstream) services(ctx context.Context) ([]map[string]any, error) {
	code, b, err := u.get(ctx, "/api/v1/ops/services", url.Values{})
	if err != nil {
		return nil, err
	}
	_ = code
	var list []map[string]any
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// catalog lists OCI repositories from the built-in registry /v2/_catalog.
func (u *upstream) catalog(ctx context.Context) ([]string, error) {
	code, b, err := u.get(ctx, "/v2/_catalog", url.Values{})
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("catalog: %d", code)
	}
	var out struct {
		Repositories []string `json:"repositories"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out.Repositories, nil
}

// releases lists releases across repos (Lab releases), or a single repo.
func (u *upstream) releases(ctx context.Context, ns, repo string) ([]map[string]any, error) {
	code, b, err := u.get(ctx, "/api/v1/repo/"+ns+"/"+repo+"/releases", url.Values{})
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("releases: %d", code)
	}
	var list []map[string]any
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	return list, nil
}
