// Wire codec (plan 2026-11-22-relay-federation task 2): every frame type
// must round-trip through Encode/Decode, binary payloads must ride as
// base64, and the envelope peek must expose t/id for routing.
package fed_test

import (
	"encoding/json"
	"strings"
	"testing"

	"llm-relay/internal/fed"
)

func TestCodecRoundTrip(t *testing.T) {
	cases := []fed.Frame{
		&fed.Hello{T: fed.THello, Proto: 1, Name: "spoke-a", Version: "0.4.2"},
		&fed.HelloAck{T: fed.THelloAck, Proto: 1, Name: "hub-a", Version: "0.4.2"},
		&fed.Catalog{T: fed.TCatalog, Models: []fed.CatalogModel{
			{Name: "qwen", UpstreamModel: "qwen-up", PriceIn: 100, PriceOut: 200, ContextLength: 262144},
		}},
		&fed.Status{T: fed.TStatus, Online: []int64{2, 3}, Offline: []int64{4}},
		&fed.Req{T: fed.TReq, ID: "r1", Method: "POST", Path: "/v1/chat/completions",
			Headers: fed.Headers{{"Content-Type", "application/json"}, {"X-Dup", "a"}, {"X-Dup", "b"}},
			Body:    []byte{0x00, 0x01, 0x02, 0xff}},
		&fed.Res{T: fed.TRes, ID: "r1", Status: 200,
			Headers: fed.Headers{{"Content-Type", "text/event-stream"}}},
		&fed.ResBody{T: fed.TResBody, ID: "r1", Data: []byte{0xde, 0xad, 0xbe, 0xef}},
		&fed.ResEnd{T: fed.TResEnd, ID: "r1", Error: "upstream died"},
		&fed.Ping{T: fed.TPing},
		&fed.Pong{T: fed.TPong},
	}
	for _, want := range cases {
		data, err := fed.Encode(want)
		if err != nil {
			t.Fatalf("encode %T: %v", want, err)
		}
		got, err := fed.Decode(data)
		if err != nil {
			t.Fatalf("decode %T (%s): %v", want, data, err)
		}
		if got.FrameType() != want.FrameType() {
			t.Errorf("type = %s, want %s", got.FrameType(), want.FrameType())
		}
		a, _ := json.Marshal(got)
		b, _ := json.Marshal(want)
		if string(a) != string(b) {
			t.Errorf("round-trip %s: %s != %s", want.FrameType(), a, b)
		}
	}
}

func TestCodecBinaryIsBase64(t *testing.T) {
	// The documented choice: []byte fields marshal as base64 strings.
	data, err := fed.Encode(&fed.ResBody{T: fed.TResBody, ID: "r1", Data: []byte{0, 1, 2}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"data":"AAEC"`) {
		t.Errorf("body chunk not base64 on the wire: %s", data)
	}
}

func TestCodecPeek(t *testing.T) {
	typ, id, err := fed.Peek([]byte(`{"t":"res_body","id":"r7","data":"AAA="}`))
	if err != nil || typ != "res_body" || id != "r7" {
		t.Errorf("peek = %q %q %v", typ, id, err)
	}
	if _, _, err := fed.Peek([]byte(`{"id":"x"}`)); err == nil {
		t.Error("frame without t accepted")
	}
}

func TestCodecUnknownType(t *testing.T) {
	if _, err := fed.Decode([]byte(`{"t":"from_the_future"}`)); err == nil {
		t.Error("unknown frame type decoded")
	}
}
