// Build-version awareness (plan 2026-11-25-version-awareness): the relay
// publishes its own version, and warns when a connected instance (spoke,
// hub, remote backend) runs a different one. Two surfaces, per the owner:
// THIS server being the older one is a red banner for ALL users (gated by
// the version_warn setting) plus an error line in the log (emitted at
// handshake time in fed/remote); a CONNECTED instance being older is only
// a small note on the admin Federation page.
package api

import (
	"database/sql"
	"errors"
	"net/http"

	"llm-relay/internal/auth"
	"llm-relay/internal/store"
	"llm-relay/internal/version"
)

// versionWarnSetting gates the red dashboard banner. Absent = enabled:
// the warning is on until an admin explicitly mutes it (the log lines and
// the Federation notes are never gated).
func versionWarnEnabled(db *sql.DB) bool {
	v, err := store.GetSetting(db, "version_warn")
	if errors.Is(err, store.ErrNotFound) {
		return true
	}
	return err == nil && v != "0"
}

// versionWarnings aggregates the live-channel version mismatches from
// the federation manager and the remote-backend registry. Either side
// may be unwired (nil controller) — standalone boxes see nothing.
func (s *Server) versionWarnings() []version.Warn {
	out := []version.Warn{}
	if s.Fed != nil {
		out = append(out, s.Fed.VersionWarnings()...)
	}
	if s.Remote != nil {
		out = append(out, s.Remote.VersionWarnings()...)
	}
	return out
}

// ServeVersion serves GET /api/version (session auth, ANY user): this
// build's version plus the outdated verdict that drives the red banner.
// outdated is true when a connected instance runs a NEWER version — this
// server is the old one. warn_enabled carries the admin setting so the UI
// can hide the banner without a second round-trip.
func (s *Server) ServeVersion(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	warns := s.versionWarnings()
	newer := []version.Warn{}
	for _, wn := range warns {
		if wn.State == version.Newer {
			newer = append(newer, wn)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":      s.Version,
		"outdated":     len(newer) > 0,
		"newer_than":   newer,
		"warn_enabled": versionWarnEnabled(s.DB),
	})
}
