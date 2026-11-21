// Agent-facing utilization: "is this model free right now?" Built from the
// same pieces the dashboard uses — in-flight counts, the health cache and
// busy-time buckets — but always unscoped: whether the server is free is
// global truth, not per-user state (plan decision: no per-user scoping
// here). Served as REST (/v1/utilization) and as one MCP tool.
package proxy

import (
	"context"
	"math"
	"net/http"
	"sort"
	"time"

	"llm-relay/internal/store"
)

// utilWindowMinutes is the averaging window for utilization_pct: long enough
// to not be twitchy, short enough to answer "free now" (plan decision 4).
const utilWindowMinutes = 5

// busyPctThreshold is the mean-busy percentage at or above which a model
// counts as busy even when the concurrency limit is unset or not reached.
const busyPctThreshold = 80.0

// ModelStatus is one model's delegation-readiness snapshot.
type ModelStatus struct {
	Name             string  `json:"name"`
	Online           bool    `json:"online"`
	Active           int     `json:"active"`
	ConcurrencyLimit int64   `json:"concurrency_limit"`
	UtilizationPct   float64 `json:"utilization_pct"`
	// Status is the derived verdict: "offline" (backend unreachable),
	// "busy" (all slots in use or mean busy >= 80 %), else "free".
	Status string `json:"status"`
}

// Utilization builds the payload shared by GET /v1/utilization and the MCP
// check_utilization tool.
func (h *Handler) Utilization(ctx context.Context) (map[string]any, error) {
	backends, err := store.ListBackends(h.DB)
	if err != nil {
		return nil, err
	}
	models, err := store.ListModels(h.DB)
	if err != nil {
		return nil, err
	}
	// Mean busy % per model over the window: total busy ms / window ms.
	// Models with no traffic in the window simply keep 0.
	now := time.Now()
	busy, err := store.BusyByModel(h.DB, &store.UsageFilter{}, now, utilWindowMinutes)
	if err != nil {
		return nil, err
	}
	busyPct := map[string]float64{}
	for _, mb := range busy {
		var total int64
		for _, ms := range mb.Values {
			total += ms
		}
		busyPct[mb.Model] = float64(total) / (float64(len(mb.Values)) * 60000) * 100
	}

	status := h.Health.Status(ctx, backends)
	active := h.Flight.Snapshot(0)

	out := []ModelStatus{}
	for _, m := range models {
		if !m.Enabled {
			continue
		}
		ms := ModelStatus{
			Name:             m.Name,
			Active:           active[m.Name],
			ConcurrencyLimit: m.ConcurrencyLimit,
			UtilizationPct:   math.Round(busyPct[m.Name]*10) / 10,
		}
		ms.Online = status[m.BackendID].Online
		switch {
		case !ms.Online:
			ms.Status = "offline"
		case m.ConcurrencyLimit > 0 && ms.Active >= int(m.ConcurrencyLimit):
			ms.Status = "busy"
		case ms.UtilizationPct >= busyPctThreshold:
			ms.Status = "busy"
		default:
			ms.Status = "free"
		}
		out = append(out, ms)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	return map[string]any{
		"window_minutes": utilWindowMinutes,
		"models":         out,
	}, nil
}

// UtilizationREST serves GET /v1/utilization (relay port, sk-key auth).
func (h *Handler) UtilizationREST(w http.ResponseWriter, r *http.Request) {
	if _, _, err := h.authKey(r); err != nil {
		writeError(w, http.StatusUnauthorized, "invalid API key")
		return
	}
	payload, err := h.Utilization(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, payload)
}
