// Federation integration in the proxy (plan 2026-11-22-relay-federation,
// task 3). Three seams, one per direction:
//
//   - Outbound: a scheme-dispatching RoundTripper on the shared Client —
//     peer:// and hub:// backends are tunneled through the fed channel by
//     the fed.Manager (FedTransport below); every http(s) backend keeps the
//     inner transport untouched.
//   - Inbound: the fed frame server runs a tunneled request through this
//     same pipeline with a FedServe identity injected into the context —
//     the peer's owner user on the hub, an anonymous no-meter identity on a
//     spoke serving the hub's traffic.
//   - Gates: a request carrying OriginPeer is checked against the sharing
//     rules (403) and never routed back to its origin (loop guard); a
//     spoke-side served request (LocalOnly) never re-forwards into the
//     federation.
package proxy

import (
	"context"
	"database/sql"
	"net/http"
	"strings"

	"llm-relay/internal/store"
)

// FedTransport is the fed.Manager's outbound tunnel surface (implemented by
// *fed.Manager; nil on a standalone relay). RoundTripFed answers one
// peer:// or hub:// request and must return before any response byte is
// consumed, so the group retry loop can still fail over on its errors.
type FedTransport interface {
	RoundTripFed(req *http.Request) (*http.Response, error)
}

// schemeDispatch routes synthetic schemes to the federation tunnel and
// everything else to the inner transport. It is installed as the shared
// Client's Transport so group members and plain models dispatch identically.
type schemeDispatch struct {
	inner http.RoundTripper
	h     *Handler
}

func (d *schemeDispatch) RoundTrip(req *http.Request) (*http.Response, error) {
	if d.h.Fed != nil && isFedScheme(req.URL.Scheme) {
		return d.h.Fed.RoundTripFed(req)
	}
	return d.inner.RoundTrip(req)
}

func isFedScheme(scheme string) bool {
	return scheme == "peer" || scheme == "hub"
}

// isSyntheticURL reports whether a backend base_url names a federation
// location (peer://<id> / hub://…) instead of a network address: the health
// prober must never dial these, the tunnel owns them.
func isSyntheticURL(baseURL string) bool {
	_, _, ok := store.SyntheticBaseURL(baseURL)
	return ok
}

// joinBackendURL builds the upstream URL for one request path. The plain
// TrimRight would eat the slashes of a synthetic base_url ("hub://" →
// "hub:"), so synthetic bases are appended verbatim.
func joinBackendURL(baseURL, path string) string {
	if _, _, ok := store.SyntheticBaseURL(baseURL); ok {
		return baseURL + path
	}
	return strings.TrimRight(baseURL, "/") + path
}

// upstreamFor picks the model name to rewrite the forwarded body to. A
// federated (synthetic) backend is a peer's relay, not an engine: the
// serving side resolves the request by the NAME its own catalog carries
// and applies its own upstream mapping locally — so the wire name stays
// the catalog name. Every http(s) backend gets the admin's upstream id as
// always.
func upstreamFor(cand store.GroupMember) string {
	if cand.Backend != nil {
		if _, _, syn := store.SyntheticBaseURL(cand.Backend.BaseURL); syn {
			return cand.Model.Name
		}
	}
	return cand.Model.UpstreamModel
}

// FedServe is the identity a tunneled request carries through the pipeline
// (set by the fed frame server via WithFedServe). It replaces API-key
// authentication for the duration of one served request.
type FedServe struct {
	// User/Key are the identity metering attributes: on the hub, the peer's
	// owner user and a synthetic "federation" key; on a spoke serving the
	// hub, an anonymous pair that is never recorded (NoMeter).
	User *store.User
	Key  *store.APIKey
	// OriginPeer is the requesting peer as the receiver sees it (0 = not a
	// peer request): the hub's sharing gate and the loop-guard exclusion
	// both key on it.
	OriginPeer int64
	// LocalOnly restricts member selection to local http(s) backends: a
	// spoke serving a frame never re-forwards into the federation.
	LocalOnly bool
	// NoMeter suppresses the usage row (spoke serving side, task 3 scope:
	// attribution columns land with task 4).
	NoMeter bool
}

type fedServeKey struct{}

// WithFedServe attaches the served identity to a request context.
func WithFedServe(ctx context.Context, fs *FedServe) context.Context {
	return context.WithValue(ctx, fedServeKey{}, fs)
}

// FedServeFrom returns the served identity, nil for ordinary traffic.
func FedServeFrom(ctx context.Context) *FedServe {
	fs, _ := ctx.Value(fedServeKey{}).(*FedServe)
	return fs
}

// fedMemberFilter decides which group members a served (tunneled) request
// may use: the loop guard (never route back to the origin peer) and, on a
// spoke, LocalOnly (never re-forward to the hub or another peer). Returns
// nil when no filter applies (ordinary local traffic).
func fedMemberFilter(fs *FedServe) func(mem store.GroupMember) bool {
	if fs == nil {
		return nil
	}
	keepLocalOnly := fs.LocalOnly
	var origin int64
	if fs.OriginPeer != 0 {
		origin = fs.OriginPeer
	}
	if !keepLocalOnly && origin == 0 {
		return nil
	}
	return func(mem store.GroupMember) bool {
		if mem.Backend == nil {
			return true
		}
		kind, peerID, syn := store.SyntheticBaseURL(mem.Backend.BaseURL)
		if !syn {
			return true
		}
		if keepLocalOnly {
			return false
		}
		// Hub side serving a peer request: a member on the ORIGIN's own
		// backend would route the request straight back where it came from.
		if kind == store.FedSchemePeer && peerID == origin {
			return false
		}
		return true
	}
}

// fedShareVisible reports whether a served peer request may use one model
// row: local rows must be explicitly shared with the peer; materialized
// rows are shareable to everyone except their own origin (the hub relays a
// spoke's models to the rest of the federation — that is the merged pool).
func fedShareVisible(db *sql.DB, mem store.GroupMember, origin int64) bool {
	if mem.Model == nil || mem.Backend == nil {
		return false
	}
	kind, peerID, syn := store.SyntheticBaseURL(mem.Backend.BaseURL)
	if syn {
		return kind == store.FedSchemePeer && peerID != origin
	}
	ok, err := store.SharedWithPeer(db, mem.Model.ID, origin)
	return err == nil && ok
}
