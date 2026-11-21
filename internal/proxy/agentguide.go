// Public agent guide: plain markdown served without auth so an agent can
// be pointed at the URL before it has any token — learning how to get a
// token is the guide's first job (plan decision 2).
package proxy

import (
	_ "embed"
	"io"
	"net/http"
	"strings"
)

//go:embed agent-guide.md
var agentGuideMD string

// AgentGuide serves GET /v1/agent-guide (relay port, no auth).
func (h *Handler) AgentGuide(w http.ResponseWriter, r *http.Request) {
	// RelayURL/DashboardURL are server roots (config convention: no /v1
	// suffix). Unset values degrade visibly instead of silently: an empty
	// relay keeps the {{RELAY_URL}} placeholder (documented contract) and
	// an empty dashboard drops the parenthetical, so the guide never
	// instructs an agent to call a relative or empty URL.
	md := agentGuideMD
	if base := strings.TrimRight(h.RelayURL, "/"); base != "" {
		md = strings.ReplaceAll(md, "{{RELAY_URL}}", base)
	}
	dash := ""
	if d := strings.TrimRight(h.DashboardURL, "/"); d != "" {
		dash = " (`" + d + "`)"
	}
	md = strings.ReplaceAll(md, "{{DASHBOARD_URL}}", dash)
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, md)
}
