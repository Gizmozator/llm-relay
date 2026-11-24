// The connect route end to end through the real wiring (PeerAuth →
// FederationConnect → Manager role gate → WS handshake): the status
// matrix a spoke operator can hit, plus a live hello/hello_ack exchange.
package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"llm-relay/internal/fed"
)

// setFedRole writes the federation settings through the admin API (also
// proving the PUT → Manager.Reload path is live-safe).
// setFedRole PUTs the federation settings block; hubPin is the TLS
// fingerprint the spoke dials with (plan 2026-11-24-remote-tls-v1) — ""
// where the dialer never connects (dead hub_url) or the role is not spoke.
func setFedRole(t *testing.T, e *testEnv, admin, role, hubURL, hubToken, hubPin string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"role": role, "hub_url": hubURL, "hub_token": hubToken, "hub_pin": hubPin})
	resp := e.do(t, "PUT", "/api/admin/federation", string(body), admin)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set role %s = %d", role, resp.StatusCode)
	}
}

func dialConnect(t *testing.T, e *testEnv, token string) (*websocket.Conn, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/federation/v1/connect"
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPClient: e.srv.Client(), // trusts the test server's TLS cert
		HTTPHeader: http.Header{"Authorization": {"Bearer " + token}},
	})
	return c, err
}

func TestFederationConnectRoute(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	admin := login(t, e)
	alice := userToken(t, e, "alice")
	_, token := createPeer(t, e, alice, "home")

	// standalone (default): the endpoint does not exist.
	if resp := e.do(t, "GET", "/federation/v1/connect", "", token); resp.StatusCode != http.StatusNotFound {
		bodyString(t, resp)
		t.Errorf("standalone connect = %d, want 404", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if _, err := dialConnect(t, e, token); err == nil {
		t.Fatal("connect accepted on a standalone instance")
	}

	// spoke role: still no accept side here.
	setFedRole(t, e, admin, "spoke", "https://127.0.0.1:1", "fed-x", "")
	if resp := e.do(t, "GET", "/federation/v1/connect", "", token); resp.StatusCode != http.StatusNotFound {
		bodyString(t, resp)
		t.Errorf("spoke connect = %d, want 404", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// hub role: PeerAuth guards, then the handshake runs.
	setFedRole(t, e, admin, "hub", "", "", "")
	if resp := e.do(t, "GET", "/federation/v1/connect", "", "fed-wrong"); resp.StatusCode != http.StatusUnauthorized {
		bodyString(t, resp)
		t.Errorf("bad token connect = %d, want 401", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	c, err := dialConnect(t, e, token)
	if err != nil {
		t.Fatalf("hub connect: %v", err)
	}
	defer c.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	msg, _ := fed.Encode(&fed.Hello{T: fed.THello, Proto: fed.Protocol,
		Name: "spoke", Version: "0.4.2"})
	if err := c.Write(ctx, websocket.MessageText, msg); err != nil {
		t.Fatal(err)
	}
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("hello_ack: %v", err)
	}
	f, err := fed.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	ack, ok := f.(*fed.HelloAck)
	if !ok || ack.Proto != fed.Protocol {
		t.Errorf("ack = %v", f)
	}
}

func wsCloseReason(err error) string {
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		return ce.Reason
	}
	return ""
}

// hubHandshake dials the connect endpoint and completes hello/hello_ack.
func hubHandshake(t *testing.T, e *testEnv, token string) *websocket.Conn {
	t.Helper()
	c, err := dialConnect(t, e, token)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	msg, _ := fed.Encode(&fed.Hello{T: fed.THello, Proto: fed.Protocol,
		Name: "spoke", Version: "0.4.2"})
	if err := c.Write(ctx, websocket.MessageText, msg); err != nil {
		t.Fatal(err)
	}
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("hello_ack: %v", err)
	}
	f, err := fed.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f.(*fed.HelloAck); !ok {
		t.Fatalf("first reply is %T, want hello_ack", f)
	}
	return c
}

// expectChannelClosed drains reads until the socket dies and checks the
// close code + reason.
func expectChannelClosed(t *testing.T, c *websocket.Conn, wantReason string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, _, err := c.Read(ctx)
		cancel()
		if err != nil {
			if got := websocket.CloseStatus(err); got != websocket.StatusPolicyViolation {
				t.Errorf("close code = %v, want 1008", got)
			}
			if got := wsCloseReason(err); got != wantReason {
				t.Errorf("close reason = %q, want %q", got, wantReason)
			}
			return
		}
	}
	t.Fatalf("channel never closed (wanted %q)", wantReason)
}

// Item 1c (API side): disable, rotate and delete each drop the live
// channel immediately. The test env runs the Manager with production
// defaults (30 s touch throttle), so a close well inside a second proves
// the kick path, not the revocation sweep.
func TestFederationKickOnMutations(t *testing.T) {
	e := newTestEnv(t, http.NotFoundHandler())
	admin := login(t, e)
	alice := userToken(t, e, "alice")
	pid, token := createPeer(t, e, alice, "kickme")
	setFedRole(t, e, admin, "hub", "", "", "")
	peerPath := "/api/admin/peers/" + strconv.FormatInt(pid, 10)

	// 1. admin disable → "disabled".
	c := hubHandshake(t, e, token)
	resp := e.do(t, "PATCH", peerPath, `{"enabled":false}`, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("disable = %d: %s", resp.StatusCode, bodyString(t, resp))
	}
	resp.Body.Close()
	expectChannelClosed(t, c, "disabled")
	c.CloseNow()

	// Re-enable for the next round.
	resp = e.do(t, "PATCH", peerPath, `{"enabled":true}`, admin)
	resp.Body.Close()

	// 2. self-service rotate → "rotated"; the new token rides the response.
	c = hubHandshake(t, e, token)
	resp = e.do(t, "POST", "/api/peers/"+strconv.FormatInt(pid, 10)+"/rotate", "", alice)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate = %d: %s", resp.StatusCode, bodyString(t, resp))
	}
	var rot struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rot); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	expectChannelClosed(t, c, "rotated")
	c.CloseNow()
	if rot.Token == "" || rot.Token == token {
		t.Fatal("rotate returned no fresh token")
	}

	// 3. admin delete, channel live on the rotated token → "deleted".
	c = hubHandshake(t, e, rot.Token)
	resp = e.do(t, "DELETE", peerPath, "", admin)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", resp.StatusCode, bodyString(t, resp))
	}
	resp.Body.Close()
	expectChannelClosed(t, c, "deleted")
	c.CloseNow()
}
