// Agent-facing utilization: "is this model free right now?" Built from the
// same pieces the dashboard uses — in-flight counts, the health cache and
// busy-time buckets — but always unscoped: whether the server is free is
// global truth, not per-user state (plan decision: no per-user scoping
// here). Served as REST (/v1/utilization) and as one MCP tool.
package proxy

import (
	"context"
	"errors"
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

// ModelStatus is one model's delegation-readiness snapshot. ID keys the
// row: group members share a name by design, so two same-name rows can
// legitimately report different status (different backends) and the
// consumer needs the physical row id to tell them apart.
type ModelStatus struct {
	ID               int64   `json:"id"`
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

// deriveStatus is the verdict switch shared by physical and pool rows:
// offline (backend unreachable) beats draining (nothing accepting new
// sessions) beats busy-by-slots, busy-by-mean-busy, else free.
func deriveStatus(online, draining bool, active int, limit int64, pct, thr float64) string {
	switch {
	case !online:
		return "offline"
	case draining:
		return "draining"
	case limit > 0 && int64(active) >= limit:
		return "busy"
	case pct >= thr:
		return "busy"
	default:
		return "free"
	}
}

// Utilization builds the payload shared by GET /v1/utilization and the MCP
// check_utilization tool. adminView selects the row list: admins get the
// physical rows (internal wiring included); non-admin keys get the
// ADVERTISED catalog — synthetic pool rows for enabled groups (negative
// ids, same convention as the dashboard live view) plus the public rows
// /v1/models actually serves. The busy/series math is always global, so a
// public model's numbers never depend on the caller's role.
func (h *Handler) Utilization(ctx context.Context, adminView bool) (map[string]any, error) {
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
	// Keyed by physical row id: with same-name group members a name-keyed
	// count would show the whole pool total on every member row and the
	// per-row busy check below (Active >= ConcurrencyLimit) would fire
	// spuriously.
	active := h.Flight.SnapshotByModel(0)

	out := []ModelStatus{}
	if adminView {
		// Admin: physical rows, unchanged (IDs, per-row wiring, sort).
		for _, m := range models {
			if !m.Enabled {
				continue
			}
			ms := ModelStatus{
				ID:               m.ID,
				Name:             m.Name,
				Active:           active[m.ID],
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
			ms.Status = deriveStatus(ms.Online, ms.Draining, ms.Active,
				m.ConcurrencyLimit, ms.UtilizationPct, float64(thr))
			out = append(out, ms)
		}
	} else {
		// Non-admin keys get the ADVERTISED catalog — the names /v1/models
		// actually serves (ListPublicCatalog) — because the agent guide
		// tells agents to check_utilization before delegating to a listed
		// name, and group membership forces every member row internal:
		// filtering physical rows alone would answer zero rows for every
		// group name. Mirrors the dashboard live view's synthesis.
		backendOn := map[int64]bool{}
		for _, b := range backends {
			backendOn[b.ID] = b.Enabled
		}
		groups, err := store.ListGroups(h.DB)
		if err != nil {
			return nil, err
		}
		poolNames := map[string]bool{}
		for _, g := range groups {
			if !g.Enabled {
				continue // a disabled group is not advertised
			}
			_, members, err := store.GetGroup(h.DB, g.ID)
			if errors.Is(err, store.ErrNotFound) {
				continue // deleted between ListGroups and GetGroup
			}
			if err != nil {
				return nil, err
			}
			var limit int64
			unknown := false
			servable := false
			online := false
			accepting := false // a servable member actually taking new sessions
			thr := int64(0)
			poolActive := 0
			// Group sessions log model_requested = the group name, so the
			// group key carries them; member-name-keyed busy (members are
			// requestable by name via the ResolveTarget fallthrough) folds
			// in too — mirror of the live view's util fold.
			poolPct := busyPct[g.Name]
			for _, mem := range members {
				if mem.Model == nil || mem.Backend == nil {
					continue
				}
				memServable := mem.Model.Enabled && backendOn[mem.Backend.ID]
				if mem.Model.Name != g.Name &&
					(mem.Model.Visibility == "internal" || memServable) {
					poolPct += busyPct[mem.Model.Name]
				}
				if !memServable {
					continue
				}
				servable = true
				poolActive += active[mem.Model.ID]
				limit += mem.Model.ConcurrencyLimit
				if mem.Model.ConcurrencyLimit == 0 {
					unknown = true
				}
				if status[mem.Backend.ID].Online {
					online = true
					if !draining[mem.Backend.ID] {
						accepting = true
					}
				}
				// Threshold aggregate: the STRICTEST member threshold. The
				// pool pct is one number, and "any member below its own
				// threshold ⇒ not busy" (one free member means the name
				// can take work) reduces to pct >= max(thresholds).
				mthr := mem.Model.BusyThresholdPct
				if mthr <= 0 {
					mthr = int64(busyPctThreshold)
				}
				if mthr > thr {
					thr = mthr
				}
			}
			if !servable {
				continue // nothing servable: the catalog does not advertise it either
			}
			if unknown {
				limit = 0 // any-unset ⇒ unknown, same rule as the live view
			}
			poolNames[g.Name] = true
			pct := math.Round(poolPct*10) / 10
			ms := ModelStatus{
				ID:               -g.ID, // negative = synthetic, same convention as the live view
				Name:             g.Name,
				Online:           online,
				Draining:         !accepting, // nothing servable is actually accepting
				Active:           poolActive,
				ConcurrencyLimit: limit,
				UtilizationPct:   pct,
			}
			ms.Status = deriveStatus(ms.Online, ms.Draining, ms.Active, limit, pct, float64(thr))
			out = append(out, ms)
		}
		// Public standalone rows, same rules as ListPublicModels: enabled
		// model on an enabled backend, public visibility, same-name dedupe
		// keeping the ResolveTarget winner (all rows here are public, so
		// lowest id wins) — minus rows whose name an emitted pool row
		// carries (the group shadows them; the standalone cannot serve it).
		best := map[string]int64{}
		for _, m := range models {
			if !m.Enabled || m.Visibility != "public" || !backendOn[m.BackendID] {
				continue
			}
			if id, ok := best[m.Name]; !ok || m.ID < id {
				best[m.Name] = m.ID
			}
		}
		for _, m := range models {
			if !m.Enabled || m.Visibility != "public" || !backendOn[m.BackendID] {
				continue
			}
			if poolNames[m.Name] || best[m.Name] != m.ID {
				continue
			}
			ms := ModelStatus{
				ID:               m.ID,
				Name:             m.Name,
				Active:           active[m.ID],
				ConcurrencyLimit: m.ConcurrencyLimit,
				UtilizationPct:   math.Round(busyPct[m.Name]*10) / 10,
			}
			ms.Online = status[m.BackendID].Online
			ms.Draining = draining[m.BackendID]
			thr := m.BusyThresholdPct
			if thr <= 0 {
				thr = int64(busyPctThreshold)
			}
			ms.Status = deriveStatus(ms.Online, ms.Draining, ms.Active,
				m.ConcurrencyLimit, ms.UtilizationPct, float64(thr))
			out = append(out, ms)
		}
	}
	// Name order with an id tie-break: same-name group members otherwise
	// land in an unspecified order (ListModels guarantees only ORDER BY
	// name), which would make the row list unstable across calls.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})

	return map[string]any{
		"window_minutes": utilWindowMinutes,
		"models":         out,
	}, nil
}

// UtilizationREST serves GET /v1/utilization (relay port, sk-key auth).
func (h *Handler) UtilizationREST(w http.ResponseWriter, r *http.Request) {
	_, user, err := h.authKey(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid API key")
		return
	}
	payload, err := h.Utilization(r.Context(), user.Role == "admin")
	if err != nil {
		log.Printf("relay: /v1/utilization failed: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, payload)
}
