// Live dashboard endpoint: recent per-minute traffic, in-flight streams and
// model online status in one poll. The view is server-wide for every logged-in
// user — its job is answering "is the server free" (owner decision); only the
// admin route accepts a user_id drill-down. Cost is not exposed here at all.
package api

import (
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"llm-relay/internal/auth"
	"llm-relay/internal/proxy"
	"llm-relay/internal/store"
)

// FlightSource exposes the proxy's in-flight counts (proxy.InFlight).
// Snapshot(0) returns all users' counts; a non-zero userID scopes to one.
type FlightSource interface {
	Snapshot(userID int64) map[string]int
}

// healthStatus returns the lazily created shared health cache (the probe
// rules live in internal/proxy so the agent endpoints use the same ones).
func (s *Server) healthStatus() *proxy.HealthCache {
	s.healthOnce.Do(func() { s.health = proxy.NewHealthCache() })
	return s.health
}

// LiveModel is one catalog model with its live status.
type LiveModel struct {
	Name    string `json:"name"`
	Backend string `json:"backend"`
	Online  bool   `json:"online"`
	Active  int    `json:"active"`
}

// Live serves /api/live and /api/admin/live.
func (s *Server) Live(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	minutes := 60
	if v := r.URL.Query().Get("window"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad window")
			return
		}
		minutes = n
	}
	if minutes < 5 {
		minutes = 5
	}
	if minutes > 360 {
		minutes = 360
	}

	// The live view is server-wide for every logged-in user: the whole
	// point is that anyone can see whether the server is free (owner
	// decision, supersedes the v1 per-user scoping). The admin route
	// keeps user_id as a drill-down; the personal route ignores it.
	// Privilege comes from the claims (same rule as usageScope), not
	// from the URL alone, so the drill-down cannot leak if routes move.
	f := &store.UsageFilter{}
	if c.Role == "admin" && strings.HasPrefix(r.URL.Path, "/api/admin/") {
		if v := r.URL.Query().Get("user_id"); v != "" {
			id, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "bad user_id")
				return
			}
			f.UserID = id
		}
	}
	// One clock for the whole response: buckets and busy windows must align.
	now := time.Now()
	buckets, err := store.LiveBuckets(s.DB, f, now, minutes)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	busy, err := store.BusyByModel(s.DB, f, now, minutes)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	backends, err := store.ListBackends(s.DB)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	models, err := store.ListModels(s.DB)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	status := s.healthStatus().Status(r.Context(), backends)
	// Active streams are server-wide (Snapshot(0)); the user_id filter
	// narrows them only on the admin route, where f.UserID is set.
	active := s.activeByModel(f.UserID)

	liveModels := []LiveModel{}
	for _, m := range models {
		if !m.Enabled {
			continue
		}
		lm := LiveModel{Name: m.Name, Backend: m.BackendName, Active: active[m.Name]}
		if e, ok := status[m.BackendID]; ok {
			lm.Online = e.Online
		}
		liveModels = append(liveModels, lm)
	}
	sort.Slice(liveModels, func(i, j int) bool { return liveModels[i].Name < liveModels[j].Name })

	type activeRow struct {
		Model string `json:"model"`
		Count int    `json:"count"`
	}
	rows := []activeRow{}
	for m, n := range active {
		rows = append(rows, activeRow{Model: m, Count: n})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Model < rows[j].Model })

	// Utilization: busy ms per minute as % of the minute (parallel streams
	// stack, so >100 % means multiple concurrent generations). The model's
	// concurrency_limit lets the frontend scale the axis to the slots.
	limits := map[string]int64{}
	for _, m := range models {
		limits[m.Name] = m.ConcurrencyLimit
	}
	type utilRow struct {
		Model            string    `json:"model"`
		Values           []float64 `json:"values"`
		ConcurrencyLimit int64     `json:"concurrency_limit"`
	}
	util := []utilRow{}
	for _, mb := range busy {
		vals := make([]float64, len(mb.Values))
		for i, ms := range mb.Values {
			vals[i] = math.Round(float64(ms)/60) / 10 // ms → % of 60 s, 1 decimal
		}
		util = append(util, utilRow{Model: mb.Model, Values: vals, ConcurrencyLimit: limits[mb.Model]})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"buckets": buckets,
		"active":  rows,
		"models":  liveModels,
		"util":    util,
	})
}

func (s *Server) activeByModel(userID int64) map[string]int {
	if s.Flight == nil {
		return map[string]int{}
	}
	return s.Flight.Snapshot(userID)
}
