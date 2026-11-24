package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// probeBackend returns a server whose /health answers healthBody (HTTP 200)
// and whose /v1/models always lists one model.
func probeBackend(t *testing.T, healthBody string) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.Write([]byte(healthBody))
		case "/v1/models":
			w.Write([]byte(`{"data":[{"id":"m"}]}`))
		}
	}))
	t.Cleanup(up.Close)
	return up
}

// Real backends report /health version in different shapes: a plain string
// or an object (halogen-style api/engine/match). The probe must record the
// version when it can — and never let the version affect the model list.
func TestProbeHealthVersionShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"string version", `{"version":"1.2.3"}`, "1.2.3"},
		{"object prefers api over engine", `{"version":{"api":"0.20.7","engine":"0.19.1","match":false}}`, "0.20.7"},
		{"object engine only", `{"version":{"engine":"0.20.7","match":true}}`, "0.20.7"},
		{"object without api or engine", `{"version":{"match":true}}`, ""},
		{"no version field", `{"status":"ok"}`, ""},
		{"null version", `{"version":null}`, ""},
		{"numeric version", `{"version":5}`, ""},
		{"malformed body", `<html>not json</html>`, ""},
		{"empty body", ``, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := probeBackend(t, tc.body)
			res := Probe(context.Background(), up.URL, "")
			if res.Version != tc.want {
				t.Errorf("version = %q, want %q", res.Version, tc.want)
			}
			// Informational only: whatever /health said, the model list
			// must still land complete.
			if !res.OK || len(res.Models) != 1 || res.Models[0] != "m" {
				t.Errorf("probe must still list models regardless of /health shape: %+v", res)
			}
		})
	}
}

// A /health body no parser can read must not abort or empty the probe: the
// version is dropped, the model list is returned as if /health were silent.
func TestProbeVersionFailureNeverBlocksModels(t *testing.T) {
	up := probeBackend(t, `}{ not json`)
	res := Probe(context.Background(), up.URL, "")
	if res.Version != "" {
		t.Errorf("version = %q from a malformed /health, want empty", res.Version)
	}
	if res.Error != "" {
		t.Errorf("error = %q, want empty", res.Error)
	}
	if !res.OK || len(res.Models) != 1 || res.Models[0] != "m" {
		t.Errorf("model list blocked by a version parse failure: %+v", res)
	}
}
