package store

// Model groups: a public name (e.g. qwen3-coder) backed by several model rows
// on different backends. The relay resolves the group name to one member per
// session (affinity lives in the proxy); members stay directly requestable.
// The group owns its public name: group names may not collide with a public
// model name or another group.

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ---------------------------------------------------------------- groups

type Group struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Enabled     bool   `json:"enabled"`
	MemberCount int    `json:"member_count"`
}

// GroupMember is one member row joined with its model and backend, so callers
// can decide eligibility (model/backend enabled, backend draining) without
// extra queries. Backend.ModelCount is not filled by the join.
type GroupMember struct {
	ModelID  int64    `json:"model_id"`
	Weight   float64  `json:"weight"`
	Position int      `json:"position"`
	Model    *Model   `json:"model"`
	Backend  *Backend `json:"backend"`
}

// GroupMemberInput is the admin-supplied membership row for Create/UpdateGroup.
type GroupMemberInput struct {
	ModelID  int64
	Weight   float64
	Position int
}

const groupCols = `g.id, g.name, g.enabled,
	(SELECT COUNT(*) FROM model_group_members gm WHERE gm.group_id = g.id)`

func scanGroup(row interface{ Scan(...any) error }) (*Group, error) {
	g := &Group{}
	var en int
	if err := row.Scan(&g.ID, &g.Name, &en, &g.MemberCount); err != nil {
		return nil, err
	}
	g.Enabled = en != 0
	return g, nil
}

// txLike lets a helper run on the DB or inside an open transaction.
type txLike interface {
	QueryRow(string, ...any) *sql.Row
}

// assertNoPublicModelName rejects a group name that shadows a public model
// name (any enabled state: the group would own the name once enabled).
// Group-vs-group collisions come from the UNIQUE constraint.
func assertNoPublicModelName(q txLike, name string) error {
	return assertNoPublicModelNameExcept(q, name, nil)
}

// assertNoPublicModelNameExcept is the same check with an exemption set (ids
// of models that just left the group in the same transaction: their public
// visibility is the call's own doing, not an external claim).
func assertNoPublicModelNameExcept(q txLike, name string, except map[int64]bool) error {
	var n int
	var err error
	if len(except) == 0 {
		err = q.QueryRow(`SELECT COUNT(*) FROM models WHERE name=? AND visibility='public'`, name).Scan(&n)
	} else {
		args := []any{name}
		ph := make([]string, 0, len(except))
		for id := range except {
			ph = append(ph, "?")
			args = append(args, id)
		}
		err = q.QueryRow(`SELECT COUNT(*) FROM models WHERE name=? AND visibility='public'
			AND id NOT IN (`+strings.Join(ph, ",")+`)`, args...).Scan(&n)
	}
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%w: a public model already uses the name %q", ErrExists, name)
	}
	return nil
}

// assertNoGroupName is the model-side inverse of assertNoPublicModelName: a
// name owned by a NON-EMPTY group (≥1 member) may not be taken by a model —
// the group would shadow it. Empty groups deliberately reserve nothing:
// otherwise deleting a group's last member would permanently block that
// upstream name from discovery.
func assertNoGroupName(q txLike, name string) error {
	var n int
	err := q.QueryRow(`SELECT COUNT(*) FROM model_groups g
		WHERE g.name=? AND EXISTS (SELECT 1 FROM model_group_members gm WHERE gm.group_id=g.id)`,
		name).Scan(&n)
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%w: group %q owns the name", ErrExists, name)
	}
	return nil
}

// (no reservedGroupNames helper by design: AddModels must NOT skip group
// names — a new replica's same-name row is the future member; see AddModels.)

func validGroupName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%w: group name required", ErrInvalid)
	}
	return nil
}

// CreateGroup creates an empty group, born DISABLED: a group enables only
// once it has members (UpdateGroup enforces the rule), so a half-configured
// group is never advertised. The name must not collide with a public model
// name or another group; both report ErrExists.
func CreateGroup(db *sql.DB, name string) (*Group, error) {
	if err := validGroupName(name); err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	// Guard and insert in one tx: as two autocommit statements a concurrent
	// CreateModel/AddModels could claim the same name between check and
	// write (group-vs-group collisions ride the UNIQUE constraint; the
	// group-vs-model race would not). Txs begin immediate (DSN
	// _txlock=immediate), so the write lock is held from Begin and
	// busy_timeout waits out contention instead of failing on a stale read
	// snapshot — a true conflict surfaces as a busy error only after 5s.
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := assertNoPublicModelName(tx, name); err != nil {
		return nil, err
	}
	res, err := tx.Exec(`INSERT INTO model_groups(name, enabled) VALUES(?,0)`, name)
	if err != nil {
		if isUniqueErr(err) {
			return nil, ErrExists
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return GroupByID(db, id)
}

func GroupByID(db *sql.DB, id int64) (*Group, error) {
	g, err := scanGroup(db.QueryRow(`SELECT `+groupCols+` FROM model_groups g WHERE g.id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return g, err
}

// GroupByName resolves an enabled group by its public name.
func GroupByName(db *sql.DB, name string) (*Group, error) {
	g, err := scanGroup(db.QueryRow(`SELECT `+groupCols+` FROM model_groups g WHERE g.name=? AND g.enabled=1`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return g, err
}

func ListGroups(db *sql.DB) ([]Group, error) {
	rows, err := db.Query(`SELECT ` + groupCols + ` FROM model_groups g ORDER BY g.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Group{}
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *g)
	}
	return out, rows.Err()
}

// groupMembers returns the group's members ordered by position, then model id.
func groupMembers(db *sql.DB, groupID int64) ([]GroupMember, error) {
	rows, err := db.Query(`SELECT gm.model_id, gm.weight, gm.position, `+modelCols+`, `+backendJoinCols+`
		FROM model_group_members gm
		JOIN models m ON m.id=gm.model_id
		JOIN backends b ON b.id=m.backend_id
		WHERE gm.group_id=?
		ORDER BY gm.position, gm.model_id`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// No hand-maintained width guard: scanGroupMember derives its targets
	// from modelBackendScanTargets, so query and scan move together with
	// modelCols/backendJoinCols, and row.Scan itself fails loudly on any
	// real mismatch.
	out := []GroupMember{}
	for rows.Next() {
		gm, err := scanGroupMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, gm)
	}
	return out, rows.Err()
}

func scanGroupMember(row interface{ Scan(...any) error }) (GroupMember, error) {
	var gm GroupMember
	m := &Model{}
	b := &Backend{}
	// Membership targets first, then the shared model+backend projection.
	targets, finalize := modelBackendScanTargets(m, b)
	targets = append([]any{&gm.ModelID, &gm.Weight, &gm.Position}, targets...)
	if err := row.Scan(targets...); err != nil {
		return GroupMember{}, err
	}
	finalize()
	gm.Model = m
	gm.Backend = b
	return gm, nil
}

// GetGroup returns the group together with its members (models and backends
// joined in, including enabled/draining flags so callers can filter).
func GetGroup(db *sql.DB, id int64) (*Group, []GroupMember, error) {
	g, err := GroupByID(db, id)
	if err != nil {
		return nil, nil, err
	}
	members, err := groupMembers(db, id)
	if err != nil {
		return nil, nil, err
	}
	return g, members, nil
}

// UpdateGroup applies admin edits; nil fields stay unchanged. members is a
// full replacement when non-nil (an empty non-nil slice clears the group).
// Renames re-run the collision rules; unknown group or member model ids
// report ErrNotFound, a name clash or a model already in a group ErrExists/
// ErrConflict.
func UpdateGroup(db *sql.DB, id int64, name *string, enabled *bool, members []GroupMemberInput) (*Group, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM model_groups WHERE id=?`, id).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return nil, ErrNotFound
	}
	// Pre-call state, for the shadow-transition rule in the post-condition:
	// a name-ownership check only guards calls that MOVE the group into a
	// shadowing state (enabling, or gaining its first eligible member).
	// Steady-state edits on an already-serving group must pass even when a
	// replica import left a public same-name row (AddModels deliberately
	// imports group names): that row is already shadowed by the serving
	// group, and blocking unrelated edits over it would protect nothing.
	prevEnabled := 0
	prevName := ""
	if err := tx.QueryRow(`SELECT enabled, name FROM model_groups WHERE id=?`, id).Scan(&prevEnabled, &prevName); err != nil {
		return nil, err
	}
	prevEligible := 0
	if err := tx.QueryRow(`SELECT COUNT(*) FROM model_groups g WHERE g.id=? AND `+
		eligibleMemberSQL, id).Scan(&prevEligible); err != nil {
		return nil, err
	}
	if name != nil {
		if err := validGroupName(*name); err != nil {
			return nil, err
		}
		nm := strings.TrimSpace(*name)
		if _, err := tx.Exec(`UPDATE model_groups SET name=? WHERE id=?`, nm, id); err != nil {
			if isUniqueErr(err) {
				return nil, ErrExists
			}
			return nil, err
		}
	}
	// Departed members (populated only when this call replaces membership):
	// exempt from the post-condition name check below.
	was := map[int64]bool{}
	if members != nil {
		// Membership determines visibility: a member row is hidden from
		// /v1/models (the group owns the public name) but stays directly
		// requestable; leaving a group restores public. The admin API also
		// has a manual editor (UpdateModelVisibility), so "internal and
		// memberless" can now also mean "admin hid it by hand"; either way
		// the next membership write re-asserts the rule above.
		rows, err := tx.Query(`SELECT model_id FROM model_group_members WHERE group_id=?`, id)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var mid int64
			if err := rows.Scan(&mid); err != nil {
				rows.Close()
				return nil, err
			}
			was[mid] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		// Materialized (synthetic-backend) models are owned by the
		// federation materializer: it prunes and re-creates those rows
		// without regard for any group, so an admin membership over them
		// can never be stable.
		if err := assertNoSyntheticMembers(tx, members); err != nil {
			return nil, err
		}
		seen := map[int64]bool{}
		if _, err := tx.Exec(`DELETE FROM model_group_members WHERE group_id=?`, id); err != nil {
			return nil, err
		}
		for _, mem := range members {
			if seen[mem.ModelID] {
				return nil, fmt.Errorf("%w: model %d listed twice", ErrConflict, mem.ModelID)
			}
			seen[mem.ModelID] = true
			var ok int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM models WHERE id=?`, mem.ModelID).Scan(&ok); err != nil {
				return nil, err
			}
			if ok == 0 {
				return nil, fmt.Errorf("%w: model %d", ErrNotFound, mem.ModelID)
			}
			// Omitted (Go zero value) or non-positive weight normalizes to
			// the default 1: a 0 weight would silently remove the member
			// from the rendezvous-hash rotation.
			w := mem.Weight
			if w <= 0 {
				w = 1
			}
			if _, err := tx.Exec(`INSERT INTO model_group_members(group_id, model_id, weight, position) VALUES(?,?,?,?)`,
				id, mem.ModelID, w, mem.Position); err != nil {
				if isUniqueErr(err) {
					return nil, fmt.Errorf("%w: model %d already belongs to a group", ErrConflict, mem.ModelID)
				}
				return nil, err
			}
		}
		if _, err := tx.Exec(`UPDATE models SET visibility='internal'
			WHERE id IN (SELECT model_id FROM model_group_members WHERE group_id=?)`, id); err != nil {
			return nil, err
		}
		for mid := range was {
			if !seen[mid] {
				if _, err := tx.Exec(`UPDATE models SET visibility='public' WHERE id=?`, mid); err != nil {
					return nil, err
				}
			}
		}
	}
	// Exemption set for both ownership checks below: departing members
	// (was) PLUS current members. A current member's row belongs to the
	// group — membership keeps it internal, but the admin visibility editor
	// may re-expose it (the plan's "admin may re-expose"); that row is not
	// an external claim on the group's name, and counting it would wedge
	// the enable path over the group's own member (409 "group name taken"
	// for a row the group legitimately owns). External public same-name
	// rows — e.g. an AddModels import — still block.
	except := map[int64]bool{}
	for mid := range was {
		except[mid] = true
	}
	mrows, err := tx.Query(`SELECT model_id FROM model_group_members WHERE group_id=?`, id)
	if err != nil {
		return nil, err
	}
	for mrows.Next() {
		var mid int64
		if err := mrows.Scan(&mid); err != nil {
			mrows.Close()
			return nil, err
		}
		except[mid] = true
	}
	if err := mrows.Err(); err != nil {
		mrows.Close()
		return nil, err
	}
	mrows.Close()
	// The rename ownership check runs after the membership flip: a call
	// that renames onto a public model's name AND claims that model as a
	// member in the same call must pass (the member is internal by then);
	// a rename that would shadow a still-public model fails here, not only
	// at enable time. Departing members (was) are exempt for the same
	// reason as the post-condition below: their public visibility is this
	// call's own flip — typical when members carry the group's future
	// name — not an external claim. A no-op rename (UI PUT echoing the
	// current name) is not a transition and must pass even when the group
	// already shadows a public same-name import row.
	if name != nil && strings.TrimSpace(*name) != prevName {
		if err := assertNoPublicModelNameExcept(tx, strings.TrimSpace(*name), except); err != nil {
			return nil, err
		}
	}
	if enabled != nil {
		v := 0
		if *enabled {
			v = 1
		}
		if v == 1 {
			// Enabling must not advertise a group that cannot serve:
			// require at least one member (the post-condition below
			// re-checks name ownership on every enabling).
			var nmem int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM model_group_members WHERE group_id=?`, id).Scan(&nmem); err != nil {
				return nil, err
			}
			if nmem == 0 {
				return nil, fmt.Errorf("%w: cannot enable a group with no members", ErrConflict)
			}
		}
		if _, err := tx.Exec(`UPDATE model_groups SET enabled=? WHERE id=?`, v, id); err != nil {
			return nil, err
		}
	}
	// Post-condition (transition-only, see prevEnabled/prevEligible): the
	// ownership check fires exactly when this call moves the group into
	// SERVING — an enable transition, or gaining the first ELIGIBLE member.
	// Only then does a public same-name model go from resolvable to
	// silently shadowed; a group that stays unable to serve shadows
	// nothing (ResolveTarget's fall-through keeps the model resolvable),
	// so edits on it must pass. Departing and current members (except) are
	// exempt: their rows are the group's own, and the fall-through handles
	// the resulting shadow deterministically (group wins while it has an
	// eligible member, the model while it does not).
	var finalName string
	var finalEnabled int
	if err := tx.QueryRow(`SELECT name, enabled FROM model_groups WHERE id=?`, id).Scan(&finalName, &finalEnabled); err != nil {
		return nil, err
	}
	finalEligible := 0
	if err := tx.QueryRow(`SELECT COUNT(*) FROM model_groups g WHERE g.id=? AND `+
		eligibleMemberSQL, id).Scan(&finalEligible); err != nil {
		return nil, err
	}
	if finalEnabled == 1 && (prevEnabled == 0 || (prevEligible == 0 && finalEligible > 0)) {
		if err := assertNoPublicModelNameExcept(tx, finalName, except); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return GroupByID(db, id)
}

// DeleteGroup removes the group and its membership rows; the member models
// stay in the catalog and become public again (they were hidden only by
// membership).
func DeleteGroup(db *sql.DB, id int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE models SET visibility='public'
		WHERE id IN (SELECT model_id FROM model_group_members WHERE group_id=?)`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM model_group_members WHERE group_id=?`, id); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM model_groups WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// ---------------------------------------------------------------- resolution

// TargetKind tells the caller what a resolved public name points at.
type TargetKind int

const (
	// TargetModel: a single catalog model; Model and Backend are set.
	TargetModel TargetKind = iota
	// TargetGroup: a model group; Group and Members are set. Picking (and
	// skipping excluded members) is the caller's job.
	TargetGroup
)

// Target is the typed result of resolving a requested model name.
type Target struct {
	Kind    TargetKind
	Name    string // the requested name (group name or model name)
	Model   *Model
	Backend *Backend
	Group   *Group
	// Members are the group's members with model+backend joined in, ordered
	// by position. All members are returned — including disabled or draining
	// ones — so the caller applies its own exclusion and ranking rules.
	Members []GroupMember
}

// ResolveTarget resolves a requested model name for public traffic:
// an enabled group by that name wins, otherwise an enabled model on an
// enabled backend (public visibility preferred, tie → lowest id).
// ErrNotFound when neither exists.
func ResolveTarget(db *sql.DB, name string) (*Target, error) {
	g, err := GroupByName(db, name)
	if err == nil {
		members, err := groupMembers(db, g.ID)
		if err != nil {
			return nil, err
		}
		if hasEligibleMember(members) {
			return &Target{Kind: TargetGroup, Name: name, Group: g, Members: members}, nil
		}
		// The group cannot serve anything (no members, or every member's
		// model or backend is disabled): fall through to the model lookup
		// instead of dead-ending the request. Draining members still count
		// as eligible here — draining is the proxy's exclusion, not this gate.
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	m, b, err := ModelByName(db, name)
	if err != nil {
		return nil, err
	}
	return &Target{Kind: TargetModel, Name: name, Model: m, Backend: b}, nil
}

// hasEligibleMember reports whether any member could serve a request right
// now: member model enabled AND its backend enabled. Go-side form of
// eligibleMemberSQL — both must express the same rule (ResolveTarget serves
// exactly what ListPublicCatalog advertises).
func hasEligibleMember(members []GroupMember) bool {
	for _, mem := range members {
		if mem.Model != nil && mem.Backend != nil && mem.Model.Enabled && mem.Backend.Enabled {
			return true
		}
	}
	return false
}

// eligibleMemberSQL is the single SQL definition of member eligibility —
// member model enabled AND its backend enabled — as a correlated EXISTS
// over a model_groups row aliased g. Shared by UpdateGroup's prevEligible
// and ListPublicCatalog's advertising filter so "advertised" ⇔ "resolvable"
// cannot drift apart.
const eligibleMemberSQL = `EXISTS (
	SELECT 1 FROM model_group_members gm
	JOIN models m ON m.id=gm.model_id
	JOIN backends b ON b.id=m.backend_id
	WHERE gm.group_id=g.id AND m.enabled=1 AND b.enabled=1)`

// ---------------------------------------------------------------- public listing

// PublicEntry is one row of the group-aware public catalog: either an enabled
// group (Model nil) or an enabled public model.
type PublicEntry struct {
	Kind  TargetKind `json:"kind"`
	Name  string     `json:"name"`
	Model *Model     `json:"model,omitempty"`
}

// ListPublicCatalog lists what /v1/models should show: enabled groups that
// have at least one ELIGIBLE member (member model enabled and its backend
// enabled — a group whose members are all disabled cannot serve, so it is
// not advertised, mirroring the model branch's enabled gates) plus enabled
// public models, ordered by name. Model entries are deduped by name keeping
// the lowest id (same tie-break as ModelByName). A group shadows a same-name
// model — the group wins, matching ResolveTarget.
func ListPublicCatalog(db *sql.DB) ([]PublicEntry, error) {
	rows, err := db.Query(`SELECT g.id, g.name FROM model_groups g
		WHERE g.enabled=1 AND ` + eligibleMemberSQL + `
		ORDER BY g.name`)
	if err != nil {
		return nil, err
	}
	out := []PublicEntry{}
	groupNames := map[string]bool{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return nil, err
		}
		groupNames[name] = true
		out = append(out, PublicEntry{Kind: TargetGroup, Name: name})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	models, err := ListPublicModels(db)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for i := range models {
		if groupNames[models[i].Name] || seen[models[i].Name] {
			continue
		}
		seen[models[i].Name] = true
		m := models[i]
		out = append(out, PublicEntry{Kind: TargetModel, Name: m.Name, Model: &m})
	}
	slices.SortFunc(out, func(a, b PublicEntry) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// assertNoSyntheticMembers rejects a membership write that would place a
// materialized (synthetic-backend) model in a group (see ErrSyntheticMember).
func assertNoSyntheticMembers(tx *sql.Tx, members []GroupMemberInput) error {
	if len(members) == 0 {
		return nil
	}
	args := make([]any, 0, len(members))
	for _, mem := range members {
		args = append(args, mem.ModelID)
	}
	rows, err := tx.Query(`SELECT m.name FROM models m JOIN backends b ON b.id=m.backend_id
		WHERE m.id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(members)), ",")+`) AND (`+syntheticWhereB+`)`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return err
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(names) > 0 {
		return fmt.Errorf("%w: %s", ErrSyntheticMember, strings.Join(names, ", "))
	}
	return nil
}
