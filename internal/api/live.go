// Live dashboard endpoint: recent per-minute traffic, in-flight streams and
// model online status in one poll. The view is server-wide for every logged-in
// user — its job is answering "is the server free" (owner decision); only the
// admin route accepts a user_id drill-down. Cost is not exposed here at all.
package api

import (
	"errors"
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

// FlightSource exposes the proxy's in-flight tracking (proxy.InFlight).
// Snapshot(0) returns all users' counts; a non-zero userID scopes to one.
// SnapshotByModel is the same counts keyed by physical model row id (group
// members share a name; per-member rows need the id view). BusySince
// returns start times of streams still running, used to credit
// in-flight busy time before the usage row exists. List returns the full
// records for the admin connections panel; Recent the ones that finished
// within the memory window (also admin connections only).
type FlightSource interface {
	Snapshot(userID int64) map[string]int
	SnapshotByModel(userID int64) map[int64]int
	BusySince(userID int64) map[string][]time.Time
	List(userID int64) []proxy.Stream
	Recent(userID int64) []proxy.Stream
}

// healthStatus returns the lazily created shared health cache (the probe
// rules live in internal/proxy so the agent endpoints use the same ones).
func (s *Server) healthStatus() *proxy.HealthCache {
	s.healthOnce.Do(func() { s.health = proxy.NewHealthCache() })
	return s.health
}

// LiveModel is one catalog model with its live status. ID keys the row:
// group members share a name by design, so the frontend must not key rows
// by name (React keys and row identity alike). On the personal route each
// enabled group with a servable member appears as ONE synthetic pool row
// (the GROUP's name, id = -groupID so it can never collide with a real
// row, pool-total Active, summed ConcurrencyLimit, zeroed Backend/Online/
// Draining) and shadows any same-name public standalone row; plain
// internal models and disabled groups stay hidden.
type LiveModel struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Backend string `json:"backend"`
	// Upstream is the engine-side model id — wiring, so it is populated
	// only on the admin route and omitted from JSON when empty.
	Upstream         string `json:"upstream,omitempty"`
	Online           bool   `json:"online"`
	Draining         bool   `json:"draining"`
	Active           int    `json:"active"`
	ConcurrencyLimit int64  `json:"concurrency_limit"`
	// Pool marks the synthetic personal-route group row (one row per
	// enabled group; member wiring is admin-only). The explicit marker
	// exists so consumers do not re-derive "is this a pool row" from
	// implicit sentinels (negative id, empty backend).
	Pool bool `json:"pool,omitempty"`
}

// modelServable is the store's eligibility rule in one place: a row serves
// traffic only when the model AND its backend are enabled. backendOn is
// keyed by backend id (ListBackends covers every backend, so group
// members' backends are in it too). The SQL copy of this rule
// (eligibleMemberSQL in the store) stays as-is by design.
func modelServable(m store.Model, backendOn map[int64]bool) bool {
	return m.Enabled && backendOn[m.BackendID]
}

// addBusy stacks src's per-minute busy values onto dst. Series are
// window-aligned; extend defensively if src is longer.
func addBusy(dst, src []int64) []int64 {
	for i, v := range src {
		if i < len(dst) {
			dst[i] += v
		} else {
			dst = append(dst, v)
		}
	}
	return dst
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
	isAdmin := c.Role == "admin" && strings.HasPrefix(r.URL.Path, "/api/admin/")
	f := &store.UsageFilter{}
	if isAdmin {
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
	//
	// Pool aggregation (plan 2026-11-22 open question, resolved here):
	// the name-keyed views — the active list below and the utilization
	// series — stay name-keyed on purpose. Streams carry the resolved
	// MEMBER name, which equals the group name when members share it (the
	// canonical setup the store seeds), so one entry per public name IS
	// the pool view the dashboard answers ("is qwen3-coder free?"). A
	// renamed member appears under its own name in the active list and in
	// in-flight busy credit; completed work still rolls up under the
	// requested name via usage_log.model_requested. The per-row
	// model list is keyed by the physical row id and counts each member's
	// OWN streams (SnapshotByModel): with same-name members, a name-keyed
	// per-row count would show the pool total on every member row and
	// double-display it.
	active := s.activeByModel(f.UserID)
	activeByID := s.activeByModelRow(f.UserID)

	draining := map[int64]bool{}
	backendOn := map[int64]bool{}
	for _, b := range backends {
		draining[b.ID] = b.Draining
		backendOn[b.ID] = b.Enabled
	}

	// Pool limit per requested name (same name-keyed view as the util
	// series below): the sum over the SERVABLE rows sharing it, because
	// group members each add their own parallel slots — a member that
	// serves nothing (model disabled OR backend disabled, the store's
	// eligibility rule) must not inflate the denominator (mirrors the
	// liveModels loop below). If any counted row with that name has no
	// limit set (0) the pool limit reports 0 — unknown — so the frontend
	// auto-scales the axis instead of topping out below the pool's real
	// capacity.
	limits := map[string]int64{}
	limitUnknown := map[string]bool{}
	for _, m := range models {
		if !modelServable(m, backendOn) {
			continue
		}
		limits[m.Name] += m.ConcurrencyLimit
		if m.ConcurrencyLimit == 0 {
			limitUnknown[m.Name] = true
		}
	}
	for name := range limits {
		if limitUnknown[name] {
			limits[name] = 0
		}
	}

	// Group-aware view for non-admins: pool rows keyed on ACTUAL group
	// membership, matching the catalog's shadowing rule (ResolveTarget /
	// ListPublicCatalog): an ENABLED group with a servable member OWNS its
	// public name, so the pool row is always emitted and any same-name
	// public standalone row is suppressed — that row is not advertised on
	// /v1/models and cannot serve the name, so the live view must not show
	// it either. Pool row: the GROUP's name, a SYNTHETIC id of -groupID
	// (real row ids are positive auto-increments, so a pool id can never
	// collide with a real row), pool-total active (summed over the
	// members' name-keyed counts) and the summed servable limit
	// (any-unset ⇒ 0), member wiring zeroed. memberNameToPool folds member
	// names into the group name in the active/util views so internal
	// member names never surface on the personal route (streams carry the
	// resolved member name) — it is built for EVERY group's internal
	// members, because a disabled group's members stay requestable by name
	// and their streams would otherwise leak the internal name; pool ROWS
	// stay enabled+servable-only. Public (re-exposed) members are NOT
	// folded: they have their own visible row and their name is what the
	// user actually requested.
	type livePool struct {
		name        string
		id          int64
		active      int
		limit       int64
		memberNames map[string]bool
	}
	var pools []livePool
	poolNames := map[string]bool{}
	memberNameToPool := map[string]string{}
	if !isAdmin {
		groups, err := store.ListGroups(s.DB)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		for _, g := range groups {
			_, members, err := store.GetGroup(s.DB, g.ID)
			if errors.Is(err, store.ErrNotFound) {
				continue // deleted between ListGroups and GetGroup (as AdminListGroups tolerates)
			}
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "internal error")
				return
			}
			// Fold map covers every group's internal members, enabled or
			// not: their names must never surface on the personal route.
			for _, mem := range members {
				if mem.Model != nil && mem.Model.Visibility == "internal" &&
					mem.Model.Name != g.Name {
					memberNameToPool[mem.Model.Name] = g.Name
				}
			}
			if !g.Enabled {
				continue // a disabled group's name is not advertised
			}
			p := livePool{name: g.Name, id: -g.ID, memberNames: map[string]bool{}}
			unknown := false
			servable := false
			for _, mem := range members {
				if mem.Model == nil || mem.Backend == nil ||
					!modelServable(*mem.Model, backendOn) {
					continue
				}
				servable = true
				p.limit += mem.Model.ConcurrencyLimit
				if mem.Model.ConcurrencyLimit == 0 {
					unknown = true
				}
				p.memberNames[mem.Model.Name] = true
			}
			if !servable {
				continue // no servable member: the catalog does not advertise it either
			}
			if unknown {
				p.limit = 0
			}
			for name := range p.memberNames {
				p.active += active[name]
			}
			// The pool sum is the ONLY limit for the name: a shadowed
			// standalone cannot serve it, so its slots must not count —
			// the folded numerator and this denominator cover the same
			// member set.
			limits[g.Name] = p.limit
			poolNames[g.Name] = true
			pools = append(pools, p)
		}
	}

	liveModels := []LiveModel{}
	for _, m := range models {
		if !m.Enabled {
			continue
		}
		// Internal rows are admin-only: member names and their backends
		// are pool wiring. Plain internal models stay hidden; group pools
		// get their row from the synthesis below.
		if !isAdmin && m.Visibility != "public" {
			continue
		}
		// A public standalone whose name an emitted pool row carries is
		// shadowed by the group (the store resolves the name to the pool,
		// not this row) — hidden for users, matching the catalog.
		if !isAdmin && poolNames[m.Name] {
			continue
		}
		lm := LiveModel{ID: m.ID, Name: m.Name, Backend: m.BackendName,
			Active: activeByID[m.ID], Draining: draining[m.BackendID],
			ConcurrencyLimit: m.ConcurrencyLimit}
		if isAdmin {
			lm.Upstream = m.UpstreamModel
		}
		if e, ok := status[m.BackendID]; ok {
			lm.Online = e.Online
		}
		liveModels = append(liveModels, lm)
	}
	// Synthetic pool rows (personal route): one per enabled group with a
	// servable member (the group owns its name — see the shadowing rule).
	if !isAdmin {
		for _, p := range pools {
			liveModels = append(liveModels, LiveModel{ID: p.id, Name: p.name,
				Active: p.active, ConcurrencyLimit: p.limit, Pool: true})
		}
	}
	// Name order with an explicit id tie-break: ListModels only guarantees
	// ORDER BY name, so same-name group members would otherwise land in an
	// unspecified (and unstable across polls) order.
	sort.Slice(liveModels, func(i, j int) bool {
		if liveModels[i].Name != liveModels[j].Name {
			return liveModels[i].Name < liveModels[j].Name
		}
		return liveModels[i].ID < liveModels[j].ID
	})

	// Internal-only names must not surface on the personal route either:
	// a plain internal model (admin flip, not a group member — so the fold
	// map above does not cover it) with live traffic would otherwise leak
	// its name through the server-wide active list and util series. A name
	// is internal-only when an internal row carries it but no public row
	// and no emitted pool row do. Names with no row at all (deleted or
	// renamed models with live streams) keep showing — recorded behavior,
	// nothing internal about a vanished row's name.
	internalOnly := map[string]bool{}
	if !isAdmin {
		for _, m := range models {
			if m.Visibility == "internal" {
				internalOnly[m.Name] = true
			}
		}
		for _, m := range models {
			if m.Visibility == "public" {
				delete(internalOnly, m.Name)
			}
		}
		for name := range poolNames {
			delete(internalOnly, name)
		}
	}

	// Personal route: fold member-name counts into the group name (streams
	// carry the resolved member name), so internal member names never
	// surface in the active list — nor, via the same map below, in the
	// util series.
	if !isAdmin {
		for member, pool := range memberNameToPool {
			if n, ok := active[member]; ok {
				active[pool] += n
				delete(active, member)
			}
		}
		for name := range internalOnly {
			delete(active, name)
		}
	}

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
	// stack, so >100 % means multiple concurrent generations). Streams
	// still running are folded in from their start time — their usage row
	// only exists at completion, and without this a long request would
	// show as a dip while running and a retroactive peak after. The
	// model's concurrency_limit lets the frontend scale the axis to the
	// slots.
	windowStart := now.UTC().Truncate(time.Minute).Add(time.Duration(-minutes+1) * time.Minute)
	series := map[string][]int64{}
	for _, mb := range busy {
		series[mb.Model] = mb.Values
	}
	for model, since := range s.activeSince(f.UserID) {
		vals, ok := series[model]
		if !ok {
			vals = make([]int64, minutes)
			series[model] = vals
		}
		proxy.FoldBusyWindows(vals, windowStart, since, now)
	}
	// Personal route: fold member-name busy series into the group name
	// (same leak as the active list; completed work already rolls up under
	// the requested name, which is the group name for group sessions).
	if !isAdmin {
		for member, pool := range memberNameToPool {
			vals, ok := series[member]
			if !ok {
				continue
			}
			delete(series, member)
			if dst, exists := series[pool]; exists {
				series[pool] = addBusy(dst, vals)
			} else {
				series[pool] = vals
			}
		}
		// Public servable members keep their own series (their name is
		// what a direct request carries) but their in-flight busy ALSO
		// credits the pool series: the pool limit sums every servable
		// member's slots, so a group session resolved to a public member
		// must not be missing from the numerator (the active map already
		// double-counts this way — pool row and member row both carry it).
		// COPY, not move: the member keeps its own entry.
		for _, p := range pools {
			for name := range p.memberNames {
				if name == p.name {
					continue // already keyed to the group name
				}
				if _, folded := memberNameToPool[name]; folded {
					continue // internal: moved into the pool above
				}
				vals, ok := series[name]
				if !ok {
					continue
				}
				if dst, exists := series[p.name]; exists {
					series[p.name] = addBusy(dst, vals)
				} else {
					series[p.name] = append([]int64(nil), vals...)
				}
			}
		}
		for name := range internalOnly {
			delete(series, name)
		}
	}
	type utilRow struct {
		Model            string    `json:"model"`
		Values           []float64 `json:"values"`
		ConcurrencyLimit int64     `json:"concurrency_limit"`
	}
	util := []utilRow{}
	for model, vals := range series {
		pct := make([]float64, len(vals))
		for i, ms := range vals {
			pct[i] = math.Round(float64(ms)/60) / 10 // ms → % of 60 s, 1 decimal
		}
		util = append(util, utilRow{Model: model, Values: pct, ConcurrencyLimit: limits[model]})
	}
	sort.Slice(util, func(i, j int) bool { return util[i].Model < util[j].Model })

	resp := map[string]any{
		"buckets": buckets,
		"active":  rows,
		"models":  liveModels,
		"util":    util,
	}
	// Admin-only detail: connection rows name other users, so they ride
	// only on the admin route (personal /api/live omits the keys). Active
	// streams plus recently-finished ones (60 s memory) so short-request
	// traffic is visible between polls, not just the frozen instant.
	if isAdmin {
		conns := []LiveConnection{}
		for _, st := range s.activeStreams(f.UserID) {
			sec := int64(now.Sub(st.Start).Seconds())
			if sec < 0 {
				sec = 0 // stream entered after the response clock was taken
			}
			conns = append(conns, LiveConnection{
				Stream:    st,
				Running:   true,
				StartedAt: st.Start.UTC().Format(time.RFC3339),
				Seconds:   sec,
			})
		}
		for _, st := range s.recentStreams(f.UserID) {
			conns = append(conns, LiveConnection{
				Stream:    st,
				Running:   false,
				StartedAt: st.Start.UTC().Format(time.RFC3339),
				EndedAt:   st.End.UTC().Format(time.RFC3339),
				Seconds:   int64(st.End.Sub(st.Start).Seconds()),
			})
		}
		sort.SliceStable(conns, func(i, j int) bool {
			return conns[i].Stream.Start.Before(conns[j].Stream.Start)
		})
		resp["connections"] = conns
		lastUsed, err := store.LastUsedByUser(s.DB, now.Add(-24*time.Hour))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		resp["last_used"] = lastUsed
	}
	writeJSON(w, http.StatusOK, resp)
}

// LiveConnection is one proxied request in the admin connections panel:
// active now, or finished within the recent window (Running=false).
// proxy.Stream carries who/what/where; started_at and seconds say how long
// it ran; ended_at dates finished rows.
type LiveConnection struct {
	proxy.Stream
	Running   bool   `json:"running"`
	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at,omitempty"`
	Seconds   int64  `json:"seconds"`
}

func (s *Server) activeStreams(userID int64) []proxy.Stream {
	if s.Flight == nil {
		return []proxy.Stream{}
	}
	return s.Flight.List(userID)
}

func (s *Server) recentStreams(userID int64) []proxy.Stream {
	if s.Flight == nil {
		return []proxy.Stream{}
	}
	return s.Flight.Recent(userID)
}

func (s *Server) activeByModel(userID int64) map[string]int {
	if s.Flight == nil {
		return map[string]int{}
	}
	return s.Flight.Snapshot(userID)
}

// activeByModelRow mirrors activeByModel keyed by physical model row id
// (per-member counts for group pools).
func (s *Server) activeByModelRow(userID int64) map[int64]int {
	if s.Flight == nil {
		return map[int64]int{}
	}
	return s.Flight.SnapshotByModel(userID)
}

// activeSince mirrors activeByModel for stream start times (in-flight busy
// credit before the usage row exists).
func (s *Server) activeSince(userID int64) map[string][]time.Time {
	if s.Flight == nil {
		return map[string][]time.Time{}
	}
	return s.Flight.BusySince(userID)
}
