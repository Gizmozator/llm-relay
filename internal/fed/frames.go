// Federation wire protocol (plan 2026-11-22-relay-federation, tasks 2–3):
// JSON text frames over one WebSocket per peer. Every frame is an object
// {"t": <type>, "id"?: <request-id>, ...payload}. Task 2 shipped
// hello/hello_ack/ping/pong; task 3 added the catalog/status/cancel handlers
// and the req/res/res_body/res_end tunnel.
//
// Binary payloads (HTTP body chunks) ride as JSON strings: Go's
// encoding/json encodes []byte as base64 automatically, so ResBody.Data
// and Req.Body are base64 on the wire. Base64 costs ~33% over raw bytes,
// which is acceptable for LAN hops and keeps the codec single-frame-type
// (text only: one code path for logging, size limits and deadlines).
package fed

import (
	"encoding/json"
	"fmt"
)

// Protocol is the wire protocol version negotiated in hello/hello_ack.
// Bump only on incompatible frame changes; a mismatched peer is refused
// (close 1002 "proto mismatch"), never silently served.
const Protocol = 1

// Frame type tags (the "t" field).
const (
	THello    = "hello"
	THelloAck = "hello_ack"
	TCatalog  = "catalog"
	TStatus   = "status"
	TReq      = "req"
	TRes      = "res"
	TResBody  = "res_body"
	TResEnd   = "res_end"
	TCancel   = "cancel"
	TPing     = "ping"
	TPong     = "pong"
)

// MaxBody caps the raw bytes of one tunneled body frame: a request body
// (Req) or one response chunk (ResBody). The wire form is base64 (~4/3 of
// raw), and base64 of 3 MiB is exactly the 4 MiB ReadLimit, so the cap sits
// a page below 3 MiB to leave room for the JSON envelope — a peer that
// honors MaxBody can never produce a frame the read pump refuses.
const MaxBody = 3<<20 - 4<<10

// Frame is a decoded protocol frame. Every type carries its tag so a
// re-encoded frame keeps its identity.
type Frame interface {
	FrameType() string
}

// Headers ride as ordered key/value pairs (not a JSON object) so repeated
// headers survive the tunnel intact (task 3).
type Headers [][2]string

// Hello is the first frame a spoke sends after the upgrade; it names the
// instance and negotiates the protocol version.
type Hello struct {
	T       string `json:"t"`
	Proto   int    `json:"proto"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

// HelloAck is the hub's reply: its own identity plus the (agreed) proto.
type HelloAck struct {
	T       string `json:"t"`
	Proto   int    `json:"proto"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

// CatalogModel is one advertised model: everything the receiving side needs
// to materialize it as an ordinary model row (pricing, context, bench,
// concurrency meta). Names are the advertising relay's local names; ids
// never cross the wire.
type CatalogModel struct {
	Name             string  `json:"name"`
	UpstreamModel    string  `json:"upstream_model"`
	PriceIn          int64   `json:"price_in"`
	PriceOut         int64   `json:"price_out"`
	PriceCacheRead   int64   `json:"price_cache_read"`
	PriceCacheWrite  int64   `json:"price_cache_write"`
	ContextLength    int64   `json:"context_length"`
	BenchTokPerS     float64 `json:"bench_tok_per_s,omitempty"`
	ConcurrencyLimit int64   `json:"concurrency_limit,omitempty"`
	BusyThresholdPct int64   `json:"busy_threshold_pct,omitempty"`
}

// CatalogGroupMember is one group member by MODEL NAME (ids are local to
// the advertising relay): the receiving side maps names to its own rows,
// which is what makes local-first resolution fall out.
type CatalogGroupMember struct {
	ModelName string  `json:"model_name"`
	Weight    float64 `json:"weight"`
}

// CatalogGroup is one federated group: a public name over several members,
// materialized on the spoke as an ordinary (federated-marked) group.
type CatalogGroup struct {
	Name    string               `json:"name"`
	Members []CatalogGroupMember `json:"members"`
}

// Catalog advertises the sender's shared model list. Origin labels whose
// catalog the entries are: 0 = the sender's own (a spoke always sends 0;
// the hub labels relayed spoke entries with the advertising peer's id so a
// spoke can hang them off per-peer synthetic backends).
//
// Models and Groups deliberately carry NO omitempty: JSON null means "this
// frame carries no data for that section" (a partial or malformed frame —
// the receiver must skip it, never prune), while [] is a legitimate
// "shares nothing" and does prune. An omitted field would decode to nil
// and be indistinguishable from no-data, letting one failed list query
// wipe every materialized row on the peer.
type Catalog struct {
	T      string         `json:"t"`
	Origin int64          `json:"origin,omitempty"`
	Models []CatalogModel `json:"models"`
	Groups []CatalogGroup `json:"groups"`
}

// Status propagates one peer's liveness through the hub (task 3): the hub
// broadcasts it when a peer connects or its channel dies, and a spoke maps
// it onto the peer's synthetic backend (offline → disabled backend, rows
// kept so model ids survive the reconnect).
type Status struct {
	T      string `json:"t"`
	PeerID int64  `json:"peer_id"`
	Online bool   `json:"online"`
}

// Req is a tunneled HTTP request, keyed by its request id. TargetPeer 0
// means "serve here"; a non-zero id means "forward to that peer" (spoke →
// spoke transit via the hub). OriginPeer is the requesting peer — but it
// is RECEIVER-derived, never sender-trusted: each side labels a frame with
// the peer id of the channel it arrived on (the hub stamps it onward on
// transit), so a spoke cannot claim another peer's id to inherit its
// sharing rights. The loop guard never routes a request back to its
// origin, and the hub's sharing gate checks against the derived origin.
// Body is capped at MaxBody raw bytes.
type Req struct {
	T          string `json:"t"`
	ID         string `json:"id"`
	TargetPeer int64  `json:"target,omitempty"`
	OriginPeer int64  `json:"origin,omitempty"`
	// FedUser is the hub-stamped hub-side identity on whose behalf the
	// request rides: a hub-direct user's name, or the origin peer's owner
	// name for spoke-originated traffic (the hub holds that identity; a
	// spoke sends nothing — spoke-local usernames never leave the spoke).
	// A serving spoke records it on its served usage row.
	FedUser string  `json:"fed_user,omitempty"`
	Method  string  `json:"method"`
	Path    string  `json:"path"`
	Headers Headers `json:"headers,omitempty"`
	Body    []byte  `json:"body,omitempty"` // base64 on the wire
}

// Res is the response head of a tunneled request (task 3).
type Res struct {
	T       string  `json:"t"`
	ID      string  `json:"id"`
	Status  int     `json:"status"`
	Headers Headers `json:"headers,omitempty"`
}

// ResBody is one streaming response chunk (task 3); Data is base64 on the
// wire (see package comment).
type ResBody struct {
	T    string `json:"t"`
	ID   string `json:"id"`
	Data []byte `json:"data,omitempty"`
}

// ResEnd terminates a tunneled response; Error carries a transport-level
// failure message instead of a real response (task 3).
type ResEnd struct {
	T     string `json:"t"`
	ID    string `json:"id"`
	Error string `json:"error,omitempty"`
}

// Cancel aborts an in-flight tunneled request (task 3): the client sends it
// when its own consumer hung up, the hub forwards it along the transit path.
// The serving side maps the id to the served request's context cancel.
type Cancel struct {
	T  string `json:"t"`
	ID string `json:"id"`
}

// Ping is the liveness probe; any inbound frame answers it, but an
// explicit Pong keeps the channel honest when nothing else flows.
type Ping struct {
	T string `json:"t"`
}

// Pong answers Ping.
type Pong struct {
	T string `json:"t"`
}

func (Hello) FrameType() string    { return THello }
func (HelloAck) FrameType() string { return THelloAck }
func (Catalog) FrameType() string  { return TCatalog }
func (Status) FrameType() string   { return TStatus }
func (Req) FrameType() string      { return TReq }
func (Res) FrameType() string      { return TRes }
func (ResBody) FrameType() string  { return TResBody }
func (ResEnd) FrameType() string   { return TResEnd }
func (Cancel) FrameType() string   { return TCancel }
func (Ping) FrameType() string     { return TPing }
func (Pong) FrameType() string     { return TPong }

// frameFactories is the decode registry: every wire type maps to a fresh
// pointer of its Go type. Task 3's handlers dispatch off the same map.
var frameFactories = map[string]func() Frame{
	THello:    func() Frame { return &Hello{} },
	THelloAck: func() Frame { return &HelloAck{} },
	TCatalog:  func() Frame { return &Catalog{} },
	TStatus:   func() Frame { return &Status{} },
	TReq:      func() Frame { return &Req{} },
	TRes:      func() Frame { return &Res{} },
	TResBody:  func() Frame { return &ResBody{} },
	TResEnd:   func() Frame { return &ResEnd{} },
	TCancel:   func() Frame { return &Cancel{} },
	TPing:     func() Frame { return &Ping{} },
	TPong:     func() Frame { return &Pong{} },
}

// Encode marshals a frame to its wire bytes.
func Encode(f Frame) ([]byte, error) { return json.Marshal(f) }

// envelope peeks the routing fields without committing to a frame type.
type envelope struct {
	T  string `json:"t"`
	ID string `json:"id"`
}

// Peek reads the type tag and request id of a raw frame.
func Peek(data []byte) (typ, id string, err error) {
	var e envelope
	if err = json.Unmarshal(data, &e); err != nil {
		return "", "", fmt.Errorf("fed frame envelope: %w", err)
	}
	if e.T == "" {
		return "", "", fmt.Errorf("fed frame missing t field")
	}
	return e.T, e.ID, nil
}

// Decode unmarshals a raw frame into its registered Go type. Unknown type
// tags are an error at the codec level; the read pump treats them as
// forward-compat noise and ignores them (a newer peer must not kill an
// older one's channel).
func Decode(data []byte) (Frame, error) {
	typ, _, err := Peek(data)
	if err != nil {
		return nil, err
	}
	return decodeWith(typ, data)
}

// decodeWith unmarshals a frame whose type was already peeked: the hot
// path (read pump, id routing) peeks once and parses once instead of
// re-running the envelope unmarshal inside Decode.
func decodeWith(typ string, data []byte) (Frame, error) {
	factory, ok := frameFactories[typ]
	if !ok {
		return nil, fmt.Errorf("unknown fed frame type %q", typ)
	}
	f := factory()
	if err := json.Unmarshal(data, f); err != nil {
		return nil, fmt.Errorf("fed frame %s: %w", typ, err)
	}
	return f, nil
}
