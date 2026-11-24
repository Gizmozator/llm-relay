// Backend discovery: probe a backend's /health and /v1/models so the admin
// can pick which upstream models enter the catalog.
package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ProbeResult is the outcome of one backend probe.
type ProbeResult struct {
	OK      bool     `json:"ok"`
	Version string   `json:"server_version"`
	Models  []string `json:"models"`
	Error   string   `json:"error"`
}

// Probe checks reachability via /health (open even when a key is required)
// and lists model ids via /v1/models.
func Probe(ctx context.Context, baseURL, apiKey string) ProbeResult {
	// Redirects are refused, not followed: the probe carries the backend's
	// API key, and a redirect target is attacker- (or misconfiguration-)
	// chosen — following it would hand the key to an arbitrary host. The
	// 3xx surfaces as a plain "returned HTTP 30x" probe error instead.
	client := &http.Client{
		Timeout: 8 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	res := ProbeResult{Models: []string{}}

	base := strings.TrimRight(baseURL, "/")

	// 1. /health — informational (version only), so it gets its own short
	// deadline and must never delay or fail the model list probe.
	if req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil); err == nil {
		hctx, hcancel := context.WithTimeout(req.Context(), 3*time.Second)
		defer hcancel()
		if resp, err := client.Do(req.Clone(hctx)); err == nil {
			defer resp.Body.Close()
			// Only a successful answer is trusted: error pages can carry
			// arbitrary JSON bodies.
			if resp.StatusCode == http.StatusOK {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				var v struct {
					Version string `json:"version"`
				}
				if json.Unmarshal(body, &v) == nil && v.Version != "" {
					res.Version = v.Version
				}
			}
		}
	}

	// 2. /v1/models — the actual model list, with its own full deadline.
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		res.Error = "v1/models unreachable: " + err.Error()
		return res
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		res.Error = "v1/models read failed: " + err.Error()
		return res
	}
	if resp.StatusCode != http.StatusOK {
		res.Error = fmt.Sprintf("v1/models returned HTTP %d", resp.StatusCode)
		return res
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		res.Error = "v1/models returned unexpected JSON"
		return res
	}
	for _, m := range parsed.Data {
		if m.ID != "" {
			res.Models = append(res.Models, m.ID)
		}
	}
	res.OK = true
	return res
}
