// Internal test for the tunneled probe's deadline (ocr finding: an ONLINE
// connector whose engine wedges must not hold the admin request open
// forever). Lives in package api to reach probeRemote and the timeout var.
package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"llm-relay/internal/store"
)

// hangController stands in for a connected remote.Registry whose engine
// never answers: RoundTrip blocks until the request context expires and
// returns that error — exactly what the real remote.RoundTrip does on
// ctx.Done (Cancel frame + unblocked caller).
type hangController struct{}

func (hangController) ServeConn(http.ResponseWriter, *http.Request, *store.Backend, string) {
}
func (hangController) Kick(int64, string)               {}
func (hangController) BackendOnline(store.Backend) bool { return true }
func (hangController) RoundTrip(req *http.Request) (*http.Response, error) {
	<-req.Context().Done()
	return nil, req.Context().Err()
}

func TestProbeRemoteIsBounded(t *testing.T) {
	defer func(old time.Duration) { remoteProbeTimeout = old }(remoteProbeTimeout)
	remoteProbeTimeout = 100 * time.Millisecond

	s := &Server{Remote: hangController{}}
	req := httptest.NewRequest("POST", "/api/admin/backends/1/probe", nil)
	start := time.Now()
	res := s.probeRemote(req, &store.Backend{ID: 1, BaseURL: "remote://1"})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("probe hung for %s", elapsed)
	}
	if res.OK || res.Error == "" {
		t.Fatalf("wedged probe = %+v, want ok=false with a bounded error", res)
	}
}
