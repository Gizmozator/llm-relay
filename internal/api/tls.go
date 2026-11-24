// TLS certificate admin surface (plan 2026-11-24-remote-tls-v1): the
// instance identity certificate the TLS listener serves and peers pin.
// Generation is a dashboard action so a relay without a CA cert can go
// TLS in one click; a relay behind a TLS-terminating proxy with a real
// cert needs none of this (the requireTLS gate accepts X-Forwarded-Proto
// when TRUST_PROXY_HEADERS is on).
package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"llm-relay/internal/auth"
	"llm-relay/internal/store"
	"llm-relay/internal/tlscert"
)

// tlsView is what the dashboard sees. The private key never leaves the
// server; the cert PEM is attached only to the generate response (for an
// optional install into an external proxy while the fingerprint pin is
// still being set up).
type tlsView struct {
	Configured  bool     `json:"configured"`
	Fingerprint string   `json:"fingerprint,omitempty"`
	SANs        []string `json:"sans,omitempty"`
	NotAfter    string   `json:"not_after,omitempty"`
	CreatedAt   string   `json:"created_at,omitempty"`
}

func viewFromRow(t *store.RemoteTLS) tlsView {
	return tlsView{
		Configured:  true,
		Fingerprint: t.Fingerprint,
		SANs:        splitNonEmpty(t.SANs),
		NotAfter:    t.NotAfter,
		CreatedAt:   t.CreatedAt,
	}
}

func splitNonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// AdminGetTLS serves GET /api/admin/tls.
func (s *Server) AdminGetTLS(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	t, err := store.RemoteTLSGet(s.DB)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusOK, tlsView{})
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal error")
	default:
		writeJSON(w, http.StatusOK, viewFromRow(t))
	}
}

// AdminGenerateTLS serves POST /api/admin/tls {"hosts":[...]}: mint a
// self-signed cert (regenerate replaces the row; live channels are
// unaffected, new handshakes need the new pin).
func (s *Server) AdminGenerateTLS(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	var in struct {
		Hosts []string `json:"hosts"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	cert, err := tlscert.Generate(in.Hosts)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	row := &store.RemoteTLS{
		CertPEM: cert.CertPEM, KeyPEM: cert.KeyPEM, Fingerprint: cert.Fingerprint,
		SANs: strings.Join(cert.SANs, ","), NotAfter: cert.NotAfter,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := store.RemoteTLSPut(s.DB, row); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tls":      viewFromRow(row),
		"cert_pem": cert.CertPEM, // shown once: for optional external-proxy install
	})
}

// AdminDeleteTLS serves DELETE /api/admin/tls. Handshakes fail until a new
// cert exists — deliberate: no silent fallback to anything else.
func (s *Server) AdminDeleteTLS(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	if err := store.RemoteTLSDelete(s.DB); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, tlsView{})
}

// RequireTLS gates the machine-channel endpoints (federation connect/ping,
// remote connect): the fed-/rb- tokens and all tunneled traffic must never
// cross a network in the clear. Direct TLS shows as r.TLS; behind a
// TLS-terminating reverse proxy the origin sees plain HTTP plus
// X-Forwarded-Proto — trusted only with TrustProxy on AND when the relay does
// not bind its own TLS listener (SelfTLS off). A relay that serves its own TLS
// has no proxy in front of the machine channels, so a cleartext request on the
// plain port claiming X-Forwarded-Proto would be a forged bypass; peers there
// must dial the wss listener. No opt-out.
func (s *Server) RequireTLS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tlscert.ServerHandshakeOK(r) {
			next.ServeHTTP(w, r)
			return
		}
		if s.TrustProxy && !s.SelfTLS && strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
			next.ServeHTTP(w, r)
			return
		}
		writeErr(w, http.StatusForbidden,
			"TLS required: machine channels accept TLS only (direct wss, or a TLS-terminating proxy with TRUST_PROXY_HEADERS)")
	})
}
