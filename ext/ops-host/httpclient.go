package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"strings"
	"time"
)

// Shared HTTP clients: one for regular JSON calls, one long-lived for
// archive fetches/syncs that stream large payloads. Replaces the previous mix
// of per-call clients and http.DefaultClient (which has no timeout).
var (
	defaultClient = &http.Client{Timeout: 60 * time.Second}
	longClient    = &http.Client{Timeout: 15 * time.Minute}
)

// httpGetJSON fetches a URL and returns the pretty-printed JSON body.
func (s *server) httpGetJSON(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	s.addAuth(req)
	client := defaultClient
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var v interface{}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("GET %s: %d %s", redactURL(url), resp.StatusCode, toJSON(v))
	}
	return toJSON(v), nil
}

// httpGetJSONMap fetches a URL and decodes the response into a map.
func (s *server) httpGetJSONMap(ctx context.Context, url string, out *map[string]interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	s.addAuth(req)
	client := defaultClient
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var v map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("GET %s: %d %s", redactURL(url), resp.StatusCode, toJSON(v))
	}
	*out = v
	return nil
}

// httpPostJSON POSTs a JSON body and returns the pretty-printed response.
func (s *server) httpPostJSON(ctx context.Context, url string, body interface{}) (string, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	s.addAuth(req)
	client := defaultClient
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var v interface{}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("POST %s: %d %s", redactURL(url), resp.StatusCode, toJSON(v))
	}
	return toJSON(v), nil
}

// httpPostJSONMap POSTs a JSON body and decodes the response into a map.
func (s *server) httpPostJSONMap(ctx context.Context, url string, body interface{}, out *map[string]interface{}) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	s.addAuth(req)
	client := defaultClient
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var v map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("POST %s: %d %s", redactURL(url), resp.StatusCode, toJSON(v))
	}
	*out = v
	return nil
}

// httpPutJSON PUTs a JSON body and returns the pretty-printed response.
func (s *server) httpPutJSON(ctx context.Context, url string, body interface{}) (string, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	s.addAuth(req)
	client := defaultClient
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var v interface{}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("PUT %s: %d %s", redactURL(url), resp.StatusCode, toJSON(v))
	}
	return toJSON(v), nil
}

// httpPutJSONMap PUTs a JSON body and decodes the response into a map.
func (s *server) httpPutJSONMap(ctx context.Context, url string, body interface{}, out *map[string]interface{}) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	s.addAuth(req)
	client := defaultClient
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var v map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("PUT %s: %d %s", redactURL(url), resp.StatusCode, toJSON(v))
	}
	*out = v
	return nil
}

// httpDelete issues a DELETE request and ignores the (typically empty) body.
func (s *server) httpDelete(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	s.addAuth(req)
	client := defaultClient
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var v interface{}
		_ = json.NewDecoder(resp.Body).Decode(&v)
		return fmt.Errorf("DELETE %s: %d %s", redactURL(url), resp.StatusCode, toJSON(v))
	}
	return nil
}

// httpGetRaw fetches a URL and returns the raw body bytes.
func (s *server) httpGetRaw(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	s.addAuth(req)
	client := defaultClient
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return body, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return body, nil
}

// addAuth attaches the appropriate token to a request: easylab routes get the
// easylab write token (`Authorization: token <…>`); artifact-registry routes
// get the artifact token (`Authorization: Bearer <…>`). Anonymous registry
// reads are fine without a token.
func (s *server) addAuth(req *http.Request) {
	u := req.URL.String()
	isJJ := strings.HasPrefix(u, s.jj+"/") || u == s.jj
	if isJJ && s.jjToken != "" {
		req.Header.Set("Authorization", "token "+s.jjToken)
		return
	}
	if strings.HasPrefix(u, s.artifact+"/") && s.artifactToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.artifactToken)
	}
}

// redactURL strips query strings from URLs before embedding them in errors.
func redactURL(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i]
	}
	return u
}

// selfBase returns the HTTP base of this instance for self-invoking build.
func selfBase() string {
	return "http://127.0.0.1:" + envOr("ZERGX_PORT", "8080")
}

// httpPostJSONErr POSTs a JSON body and returns a plain error on failure
// (no body decoding expected).
func (s *server) httpPostJSONErr(ctx context.Context, url string, body interface{}) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	s.addAuth(req)
	resp, err := defaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("POST %s: %d %s", redactURL(url), resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return nil
}
