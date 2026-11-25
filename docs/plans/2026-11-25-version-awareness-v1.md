# Plan: Build version identity + old-version warnings

- **Status:** complete (shipped 2026-11-25)
- **Owner:** maintainer
- **Started:** 2026-11-25
- **Issue:** user request (no Gitea ticket)

## Goal

Every build carries a version that **equals its Docker image tag**, it is
visible in the dashboard, travels over every machine channel
(hub↔spoke, hub↔remote-backend — the hello frames already carry it, #16),
and a relay **warns its admin** when it is connected to anything running a
different version — in the dashboard (if enabled) and in the server log.

## Non-goals

- No update mechanism, no download checks — version awareness only.
- ~~No warnings to non-admin users (the version chip in the topbar is
  informational; the warning banner is admin-only).~~ **Superseded by the
  owner clarification below: the outdated banner shows to all users.**
- Unparseable versions (`dev`, bare shas from untagged builds) are never
  flagged — comparison needs semver.

## Design

**Version identity.** `buildVersion()` (ldflags stamp) stays the single
source. `make docker-release` currently stamps the binary with
`VERSION=latest` and tags the image `:latest` — the bug this plan fixes:
it tags `:<GIT_DESC>` **and** `:latest`, and stamps the binary with the
same `GIT_DESC`. Image tag == binary version == what the UI shows.
The repo gets its first semver tag (`v0.1.0`) at release; later builds
describe as `v0.1.0-<N>-g<sha>`.

**Comparison** (`internal/version`): parse `v?X.Y.Z(-N-g…)?` into
`(X,Y,Z,N)`; both sides parse → comparable (N = commits since the tag);
otherwise not comparable → never flagged. States: `older` (they run less
than us), `newer` (they run more), `same`, `unknown`.

**Detection** — at handshake time, both directions, live channels only:

| channel | observed at | comparison |
|---|---|---|
| spoke → hub | hub `hello` (entry.info.Version) | peer vs hub |
| hub → spoke | spoke `hello_ack` (spokeID) | hub vs spoke |
| remote backend | registry hello (entry.version) | backend vs us |

Two distinct surfaces (per the owner's clarification):

1. **This server is OLDER than something connected** → **red banner in the
   dashboard, visible to ALL users** (gated by the `version_warn`
   setting) + an error line in the server log.
2. **A CONNECTED instance is older than this server** → a **small dim
   note next to its version on the (admin) Federation page** — no banner,
   no error log (an info line at connect is enough).

Dashboard warnings are computed on demand from the live registries
(no new state): `Hub.VersionWarnings()`, spoke branch via `spokeID`,
`Registry.VersionWarnings()`.

**API**:
- `GET /api/version` (session, any user) → `{version, outdated,
  newer_than: [{name, kind, version}], warn_enabled}` — `outdated` is
  true when any connected instance runs a NEWER version (this server is
  the old one); drives the red banner for all users.
- `GET /api/admin/federation` gains `version` (self) + `version_warnings`
  (all mismatches on live channels, both directions).
- `GET /api/admin/peers` gains `version_state` per peer (vs this server)
  so the Federation table can show the small note.
- Settings gain `version_warn` (bool, default **on**): gates the red
  dashboard banner; the log lines and the Federation notes are
  unconditional.

**Web**: topbar version chip (all users); red banner under the topbar
(all pages, all users) when `outdated && warn_enabled`; Federation page —
small "older than this server" note next to a peer's version (and next to
the hub version card on a spoke); Settings gains the toggle.

## Tasks

- [x] `internal/version`: Parse/Compare/State + unit tests.
- [x] Makefile: docker-release tags `:$(GIT_DESC)` + `:latest`, stamps
  binary with GIT_DESC; initial git tag v0.1.0 at release.
- [x] `fed`: Hub.VersionWarnings (live entries), spoke-side hub check,
  handshake logging; Manager surface.
- [x] `remote`: Registry.VersionWarnings + hello logging.
- [x] `api`: Server.Version, /api/version, federation GET fields,
  peers version_state, version_warn setting.
- [x] `web`: topbar chip, Federation notes, Settings toggle.
- [x] Tests: version parse matrix, fed hub mismatch, api endpoints,
  settings round-trip.
- [x] Docs: README (version + release flow), api-contract, this plan.

## Verification

- `make ci` green.
- Prod: after redeploy, Federation page shows the running version; a
  spoke on an older build gets an `old` badge + banner on the hub;
  container log shows the mismatch line at connect.

## Decisions

- Warning covers BOTH directions (older connected = they need update;
  newer connected = we need update) — the user's ask was "warn if running
  old version", which only makes sense relative to the fleet.
- ~~Banner is admin-only (Federation page is admin surface); topbar chip is
  visible to all users (harmless, aids support).~~ **Superseded by the
  owner clarification below.**
- `version_warn` gates the dashboard banner only; the terminal log always
  fires (a headless box must still surface it in `docker logs`).
- Owner clarification (2026-11-25): the red banner fires when THIS server
  is the older one and shows to ALL users; a connected instance being
  older is only a small note on the (admin) Federation page. Implemented
  exactly that.
- `State(mine, theirs)` describes THEIRS (older = they run less), matching
  the wire `state` field everywhere.

## Open questions

- (none)

## Changelog

- 2026-11-25: plan written.
- 2026-11-25: shipped — version package, Makefile dual-tag release, fed +
  remote handshake logging + warnings, /api/version, federation GET +
  peers version_state, version_warn setting, topbar chip, red outdated
  banner, Federation notes, Settings toggle.
