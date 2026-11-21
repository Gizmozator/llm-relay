// Lightweight backend reachability check for the live dashboard. The full
// admin probe (discover.go) lists models and is too heavy to run on every
// dashboard poll; this only answers "is the server up".
package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// healthTimeout bounds one reachability check; the dashboard polls often, so
// a dead backend must fail fast.
var healthTimeout = 2 * time.Second

// Reachable GETs {base}/health and reports whether the backend is up. Any
// non-5xx HTTP answer counts as online — a 401 means the server is alive
// but keyed (same rule as the admin probe). A transport failure or any 5xx
// counts as offline: 503 is the llama.cpp-style "model not loaded", while
// 502/504 typically mean a fronting reverse proxy cannot reach the engine
// at all, which is exactly the outage this probe must surface.
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
	defer func() {
		// Drain a bounded amount so the keep-alive connection returns to
		// the pool instead of being dialed fresh on the next probe.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
	}()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("backend reports error (%d)", resp.StatusCode)
	}
	return nil
}
