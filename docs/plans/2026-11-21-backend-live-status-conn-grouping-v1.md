# Backend live status + connection grouping — v1

**Date:** 2026-11-21 · **Status:** done · **Supersedes:** —

## Problem (owner reports)

1. *"When I disabled network on this PC, llm-gateway showed status 'ok' on backends."*
   Root cause: the Backends page **Status** column renders the stored
   `last_probe_at`/`last_probe_error` fields, which are written **only** when
   the admin presses the **Probe** button (`AdminProbeBackend` →
   `store.RecordProbe` is the sole caller). The live health verdict
   (`proxy.HealthCache`, re-probe ≤ once per 60 s) feeds only the Dashboard
   model list and the agent endpoints. So the column said "ok" forever after
   the last manual probe, no matter the actual reachability.

2. *"Active connections shows multiple entries on same session."*
   Each agent turn is a separate HTTP request; with the 60 s finished-memory
   one busy session fills the panel with 5+ identical-looking rows
   (same user/key/model/endpoint/session/IP). Working as designed, but the
   panel answers "who is on which model" — that is one row per session, not
   per request.

## Approach

1. **Live status on the Backends page.** `AdminListBackends` attaches the
   shared `HealthCache` verdict per backend: `online` (bool) +
   `health_error` (string, omitempty). Status column shows the live badge
   (tooltip = probe error); the manual-probe info stays in the **Last probe**
   column (tooltip = `last_probe_error`). The page re-polls the list every
   10 s so the badge goes stale-free without a refresh (HealthCache caps the
   actual probes at one per backend per minute).
2. **Group connections per session.** Dashboard groups rows by
   `user|key|model|endpoint|session|ip`. One row per group: live timer of the
   oldest running request + `×N` when several run, `· N done` suffix for
   finished ones in the window; fully-finished groups show
   `done Xs ago ×N`, dimmed. API payload stays raw per-request rows
   (grouping is display).

## Decisions

1. `online` rides on the existing `GET /api/admin/backends` payload (view
   struct embedding `store.Backend`) — no new endpoint.
2. Disabled backends: HealthCache already reports offline "backend
   disabled" without probing — consistent with the Dashboard.
3. Grouping key includes IP: same session from two machines stays two rows.
4. The 10 s poll reuses `useLoad.reload()`; modals hold their own copies, so
   a background reload cannot disturb editing.

## Tasks

| # | task | blocked by | done |
|---|---|---|---|
| 1 | AdminListBackends live health fields | — | ☑ |
| 2 | Backends.tsx status column + 10 s poll | 1 | ☑ |
| 3 | Dashboard connection grouping | — | ☑ |
| 4 | tests (payload online true/false) | 1 | ☑ |
| 5 | docs: api-contract, README, plan | 1–3 | ☑ |

## Changelog

- 2026-11-21 — implemented as planned: `AdminListBackends` returns
  `backendView` (embeds `store.Backend` + `online` + `health_error`) from the
  shared `HealthCache`; Backends page Status = live badge (tooltip = probe
  error), Last probe column keeps the manual result (tooltip =
  `last_probe_error`), 10 s auto-poll; Dashboard connections grouped per
  user+key+model+endpoint+session+IP — running timer of the oldest live
  request, `×N` for parallel, `· N done` / `done Xs ago ×N` for finished.
  Test `TestAdminBackendsLiveStatus`: live → online, dead port → offline +
  error text, disabled → offline "backend disabled" without probe.
- 2026-11-21 — plan created.
