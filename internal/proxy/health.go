// Lightweight backend reachability check for the live dashboard. The full
// admin probe (discover.go) lists models and is too heavy to run on every
// dashboard poll; this only answers "is the server up".
package proxy

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

// healthTimeout bounds one reachability check; the dashboard polls often, so
// a dead backend must fail fast.
var healthTimeout = 2 * time.Second

// Reachable GETs {base}/health and reports whether the backend is up. Any
// HTTP answer counts as online — a 401 means the server is alive but keyed
// (same rule as the admin probe). Only a transport failure or a 503
// (llama.cpp-style "model not loaded") counts as offline.
func Reachable(ctx context.Context, baseURL string) error {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(baseURL, "/")+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusServiceUnavailable {
		return errors.New("backend reports not ready (503)")
	}
	return nil
}
