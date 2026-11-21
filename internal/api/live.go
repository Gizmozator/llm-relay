// Live dashboard endpoint: recent per-minute traffic, in-flight streams and
// model online status in one poll. Scoping follows the usage routes: the
// personal route always shows the caller their own rows; only the admin
// route is unscoped.
package api

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strconv"
	"sync"
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

// healthTTL is how long a backend reachability result is reused. Models do
// not flap on/off constantly, so one probe per minute per backend is enough
// even though the dashboard polls every few seconds (owner decision).
var healthTTL = 60 * time.Second

type healthEntry struct {
	online bool
	err    string
	at     time.Time
}

// healthCache probes backends at most once per TTL and shares the result
// across concurrent dashboard viewers.
type healthCache struct {
	mu      sync.Mutex
	entries map[int64]healthEntry
}

func (s *Server) healthStatus() *healthCache {
	s.healthOnce.Do(func() { s.health = &healthCache{entries: map[int64]healthEntry{}} })
	return s.health
}

// status returns per-backend reachability, probing only entries older than
// the TTL. Disabled backends are offline without a probe.
func (hc *healthCache) status(ctx context.Context, backends []store.Backend) map[int64]healthEntry {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	out := make(map[int64]healthEntry, len(backends))
	for _, b := range backends {
		e, fresh := hc.entries[b.ID], hc.entries[b.ID].at.After(time.Now().Add(-healthTTL))
		switch {
		case !b.Enabled:
			e = healthEntry{online: false, err: "backend disabled", at: time.Now()}
		case fresh:
			// cached result stands
		default:
			// The probe result is cached and shared for the whole TTL, so it
			// must not inherit this one viewer's request context: a client
			// disconnecting mid-probe would poison the cache with a false
			// "offline" for every viewer. Reachable bounds itself at 2 s.
			err := proxy.Reachable(context.WithoutCancel(ctx), b.BaseURL)
			e = healthEntry{online: err == nil, at: time.Now()}
			if err != nil {
				e.err = err.Error()
			}
			hc.entries[b.ID] = e
		}
		out[b.ID] = e
	}
	return out
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

	f := &store.UsageFilter{UserID: usageScope(r, c)}
	if f.UserID == 0 {
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
	status := s.healthStatus().status(r.Context(), backends)
	// Active streams follow the same scope as the buckets: the personal
	// route shows only the caller's own streams (user_id filter included).
	active := s.activeByModel(f.UserID)

	liveModels := []LiveModel{}
	for _, m := range models {
		if !m.Enabled {
			continue
		}
		lm := LiveModel{Name: m.Name, Backend: m.BackendName, Active: active[m.Name]}
		if e, ok := status[m.BackendID]; ok {
			lm.Online = e.online
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

	currency, ok := s.currencySetting()
	if !ok {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"currency": currency,
		"buckets":  buckets,
		"active":   rows,
		"models":   liveModels,
		"util":     util,
	})
}

func (s *Server) activeByModel(userID int64) map[string]int {
	if s.Flight == nil {
		return map[string]int{}
	}
	return s.Flight.Snapshot(userID)
}
