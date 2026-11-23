// Federation wire protocol (plan 2026-11-22-relay-federation, task 2):
// JSON text frames over one WebSocket per peer. Every frame is an object
// {"t": <type>, "id"?: <request-id>, ...payload}. Task 2 implements
// hello/hello_ack/ping/pong; the catalog/status/req/res types are defined
// and registered here so the codec is complete, but no handlers exist yet
// — task 3 (catalog sync + tunnel) registers them.
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
	TPing     = "ping"
	TPong     = "pong"
)

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

// CatalogModel is one advertised model in a catalog frame. Task 3 fills in
// the materialization fields it needs (pricing, context, bench); the shape
// here is deliberately minimal.
type CatalogModel struct {
	Name          string `json:"name"`
	UpstreamModel string `json:"upstream_model"`
	PriceIn       int64  `json:"price_in"`
	PriceOut      int64  `json:"price_out"`
	ContextLength int64  `json:"context_length"`
}

// Catalog advertises the sender's shared model list (task 3).
type Catalog struct {
	T      string         `json:"t"`
	Models []CatalogModel `json:"models"`
}

// Status propagates peer liveness through the hub (task 3): which peers
// came online / went offline since the last frame.
type Status struct {
	T       string  `json:"t"`
	Online  []int64 `json:"online,omitempty"`
	Offline []int64 `json:"offline,omitempty"`
}

// Req is a tunneled HTTP request (task 3), keyed by its request id.
type Req struct {
	T       string  `json:"t"`
	ID      string  `json:"id"`
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
