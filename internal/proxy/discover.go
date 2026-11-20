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
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 8 * time.Second}
	res := ProbeResult{Models: []string{}}

	base := strings.TrimRight(baseURL, "/")

	// 1. /health — any HTTP answer counts as reachable; halogen reports a
	// version here and keeps the route open even with auth enabled.
	if req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil); err == nil {
		if resp, err := client.Do(req); err == nil {
			defer resp.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			var v struct {
				Version string `json:"version"`
			}
			if json.Unmarshal(body, &v) == nil && v.Version != "" {
				res.Version = v.Version
			}
		}
	}

	// 2. /v1/models — the actual model list.
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
