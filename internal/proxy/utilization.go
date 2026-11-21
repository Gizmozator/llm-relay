// Agent-facing utilization: "is this model free right now?" Built from the
// same pieces the dashboard uses — in-flight counts, the health cache and
// busy-time buckets — but always unscoped: whether the server is free is
// global truth, not per-user state (plan decision: no per-user scoping
// here). Served as REST (/v1/utilization) and as one MCP tool.
package proxy

import (
	"context"
	"log"
	"math"
	"net/http"
	"sort"
	"time"

	"llm-relay/internal/store"
)

// utilWindowMinutes is the averaging window for utilization_pct: long enough
// to not be twitchy, short enough to answer "free now" (plan decision 4).
const utilWindowMinutes = 5

// busyPctThreshold is the default mean-busy percentage at or above which a
// model counts as busy even when the concurrency limit is unset or not
// reached; a model's busy_threshold_pct (0 = unset) overrides it.
const busyPctThreshold = 80.0

// ModelStatus is one model's delegation-readiness snapshot.
type ModelStatus struct {
	Name             string  `json:"name"`
	Online           bool    `json:"online"`
	Draining         bool    `json:"draining"`
	Active           int     `json:"active"`
	ConcurrencyLimit int64   `json:"concurrency_limit"`
	UtilizationPct   float64 `json:"utilization_pct"`
	// Status is the derived verdict: "offline" (backend unreachable),
	// "draining" (admin is shutting the backend down: finish existing
	// sessions, start none), "busy" (all slots in use or mean busy at or
	// above the model's threshold, default 80 %), else "free".
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
	// Mean busy % per model: total busy ms / elapsed window ms. The
	// divisor is the FIXED window length minus the still-running part of
	// the last bucket — BusyByModel's final bucket is the current minute,
	// and busy time can only have accumulated up to now, so dividing by
	// the full window would under-report by up to a minute's worth and
	// could keep a saturated model under the busy threshold. Values may
	// exceed 100 % (parallel streams stack); that is the intended
	// semantics, so no clamp. Mirrors BusyByModel's window-start math.
	now := time.Now()
	busy, err := store.BusyByModel(h.DB, &store.UsageFilter{}, now, utilWindowMinutes)
	if err != nil {
		return nil, err
	}
	start := now.UTC().Truncate(time.Minute).Add(-time.Duration(utilWindowMinutes-1) * time.Minute)
	elapsedMS := math.Max(float64(now.Sub(start).Milliseconds()), 1)
	// Credit streams still running: their usage row only exists once they
	// complete, so without this a long request reads as idle until the
	// moment it finishes (dip-then-retroactive-peak in the graph).
	series := map[string][]int64{}
	for _, mb := range busy {
		series[mb.Model] = mb.Values
	}
	for model, since := range h.Flight.BusySince(0) {
		vals, ok := series[model]
		if !ok {
			vals = make([]int64, utilWindowMinutes)
			series[model] = vals
		}
		FoldBusyWindows(vals, start, since, now)
	}
	busyPct := map[string]float64{}
	for model, vals := range series {
		var total int64
		for _, ms := range vals {
			total += ms
		}
		busyPct[model] = float64(total) / elapsedMS * 100
	}

	status := h.Health.Status(ctx, backends)
	draining := map[int64]bool{}
	for _, b := range backends {
		draining[b.ID] = b.Draining
	}
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
		ms.Draining = draining[m.BackendID]
		// Mean-busy threshold: per-model admin setting, 0 = default.
		thr := m.BusyThresholdPct
		if thr <= 0 {
			thr = int64(busyPctThreshold)
		}
		switch {
		case !ms.Online:
			ms.Status = "offline"
		case ms.Draining:
			ms.Status = "draining"
		case m.ConcurrencyLimit > 0 && int64(ms.Active) >= m.ConcurrencyLimit:
			ms.Status = "busy"
		case ms.UtilizationPct >= float64(thr):
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
		log.Printf("relay: /v1/utilization failed: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, payload)
}
