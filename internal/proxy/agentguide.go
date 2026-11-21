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
	// RelayURL is the server root (config convention: no /v1 suffix).
	base := strings.TrimRight(h.RelayURL, "/")
	md := strings.ReplaceAll(agentGuideMD, "{{RELAY_URL}}", base)
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, md)
}
