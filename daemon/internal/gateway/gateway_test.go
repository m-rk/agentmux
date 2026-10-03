package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/gatewayapi"
	"github.com/m-rk/agentmux/daemon/internal/ops"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
	"github.com/m-rk/agentmux/daemon/internal/transcript"
)

type fakeBackend struct {
	mu       sync.Mutex
	sessions []ops.Session
	err      error
	sent     []ops.SendRequest
	sendRes  *ops.SendResult
}

func (f *fakeBackend) Host() string { return "hostA" }
func (f *fakeBackend) List(context.Context) ([]ops.Session, error) {
	return f.sessions, f.err
}
func (f *fakeBackend) Status(_ context.Context, addr string) (ops.StatusResult, error) {
	return ops.StatusResult{Session: ops.Session{Address: addr}, State: "ready"}, f.err
}
func (f *fakeBackend) Threads(context.Context, string) ([]transcript.Thread, error) {
	return nil, f.err
}
func (f *fakeBackend) Read(_ context.Context, addr, cursor string, limit int) (transcript.Page, error) {
	return transcript.Page{}, f.err
}
func (f *fakeBackend) Send(_ context.Context, req ops.SendRequest) ops.SendResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, req)
	if f.sendRes != nil {
		return *f.sendRes
	}
	return ops.SendResult{OK: true, Address: req.Address}
}

type harness struct {
	t       *testing.T
	backend *fakeBackend
	id      Identity
	whoisN  int
	err     error
	now     time.Time
	logs    bytes.Buffer
	srv     *Server
}

func newHarness(t *testing.T, grants ...gatewayapi.Grant) *harness {
	h := &harness{t: t, backend: &fakeBackend{}, now: time.Unix(1_700_000_000, 0)}
	h.id = Identity{Node: "Orch-Box", Grants: grants}
	h.srv = New(Config{
		Backend: h.backend,
		Whois: func(context.Context, string) (Identity, error) {
			h.whoisN++
			return h.id, h.err
		},
		Logger: log.New(&h.logs, "", 0),
		Now:    func() time.Time { return h.now },
	})
	return h
}

// flush forgets cached identities, so a changed h.id takes effect.
func (h *harness) flush() {
	h.srv.whois.mu.Lock()
	h.srv.whois.entries = map[netip.Addr]cacheEntry{}
	h.srv.whois.mu.Unlock()
}

func (h *harness) do(method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "100.64.0.9:5555"
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, req)
	return rec
}

func (h *harness) post(op, body string) *httptest.ResponseRecorder {
	return h.do(http.MethodPost, gatewayapi.Path(op), body)
}

func errReason(t *testing.T, rec *httptest.ResponseRecorder) safesend.Reason {
	t.Helper()
	var e gatewayapi.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("body %q is not an ErrorResponse: %v", rec.Body, err)
	}
	return e.Error.Reason
}

func all(ops ...string) gatewayapi.Grant {
	return gatewayapi.Grant{Ops: ops, Sessions: []string{"*@*"}}
}

func TestFailsClosed(t *testing.T) {
	h := newHarness(t, all("list", "send"))
	h.err = errors.New("tailscale exploded")
	if rec := h.post("list", `{}`); rec.Code != 403 || errReason(t, rec) != safesend.ReasonForbidden {
		t.Errorf("whois error: %d %s", rec.Code, rec.Body)
	}

	h.err = nil
	h.id = Identity{Node: "!!!", Grants: []gatewayapi.Grant{all("list")}}
	h.id.Node = ""
	if rec := h.post("list", `{}`); rec.Code != 403 {
		t.Errorf("no node name: %d", rec.Code)
	}

	h.id = Identity{Node: "orch"} // no capability
	for _, op := range []string{"list", "status", "send", "read", "threads"} {
		if rec := h.post(op, `{"address":"a@hostA"}`); rec.Code != 403 {
			t.Errorf("%s with no grants: %d", op, rec.Code)
		}
	}
	if len(h.backend.sent) != 0 {
		t.Error("send reached the backend")
	}
}

func TestParseWhois(t *testing.T) {
	const cap = "example.com/cap/agentmux-gateway"
	good := `{"Node":{"Name":"Orch-Box.example.ts.net.","ComputedName":"","Tags":["tag:agentmux-host"],
	  "CapMap":{"example.com/cap/agentmux-gateway":[{"ops":["send"],"sessions":["*@*"]}]}},
	  "UserProfile":{"LoginName":"someone@example.com"},
	  "CapMap":{"example.com/cap/agentmux-gateway":[
	    {"ops":["list","read"],"sessions":["*@*"]},
	    {"ops":["send","bogus"],"sessions":["*@*"]},
	    {"ops":["send"],"sessions":[]},
	    {"ops":["send"],"sessions":["[bad"]},
	    "junk",
	    {"ops":["send"],"sessions":["mergentic*@*"]}]}}`
	id, err := ParseWhois([]byte(good), cap)
	if err != nil {
		t.Fatal(err)
	}
	if id.Node != "Orch-Box" || id.Principal() != "orch-box" || id.Login != "someone@example.com" || len(id.Tags) != 1 {
		t.Errorf("identity = %+v", id)
	}
	// Node.CapMap is the peer's own; only the top-level CapMap counts.
	if len(id.Grants) != 2 || id.Grants[1].Sessions[0] != "mergentic*@*" {
		t.Errorf("grants = %+v", id.Grants)
	}

	if id, err := ParseWhois([]byte(`{"Node":{"ComputedName":"x"},"CapMap":null}`), cap); err != nil || len(id.Grants) != 0 {
		t.Errorf("null CapMap: %+v %v", id, err)
	}
	if _, err := ParseWhois([]byte(`{"Node":{"ComputedName":"x"},"CapMap":{"`+cap+`":{"ops":["send"]}}}`), cap); err == nil {
		t.Error("non-array capability should fail")
	}
	if _, err := ParseWhois([]byte(`not json`), cap); err == nil {
		t.Error("bad json should fail")
	}
	if _, err := ParseWhois([]byte(`{"Node":{}}`), cap); err == nil {
		t.Error("no node name should fail")
	}
}

func TestSanitizePrincipal(t *testing.T) {
	for in, want := range map[string]string{
		"Orch-Box":              "orch-box",
		"my box!":               "my-box-",
		"":                      "",
		strings.Repeat("a", 80): strings.Repeat("a", 64),
	} {
		if got := sanitizePrincipal(in); got != want {
			t.Errorf("sanitizePrincipal(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAuthzGlobs(t *testing.T) {
	h := newHarness(t,
		gatewayapi.Grant{Ops: []string{"read", "status", "threads", "list"}, Sessions: []string{"*@hosta"}},
		gatewayapi.Grant{Ops: []string{"send"}, Sessions: []string{"mergentic*@*"}},
	)
	cases := []struct {
		op, addr string
		code     int
	}{
		{"status", "x@hosta", 200},
		{"status", "x@hosta#thr-1", 200}, // thread ignored for matching
		{"status", "x@hostb", 403},
		{"read", "x@hosta", 200},
		{"threads", "x@hostb", 403},
		{"status", "bad address", 400},
	}
	for _, c := range cases {
		rec := h.post(c.op, `{"address":"`+c.addr+`"}`)
		if rec.Code != c.code {
			t.Errorf("%s %s: %d %s, want %d", c.op, c.addr, rec.Code, rec.Body, c.code)
		}
	}

	send := func(addr string) gatewayapi.SendResponse {
		rec := h.post("send", `{"address":"`+addr+`","text":"hi","via":"relayed"}`)
		if rec.Code != 200 {
			t.Fatalf("send %s: %d", addr, rec.Code)
		}
		var res gatewayapi.SendResponse
		json.Unmarshal(rec.Body.Bytes(), &res)
		return res
	}
	if res := send("mergentic-opencode@hosta"); !res.OK {
		t.Errorf("granted send: %+v", res)
	}
	res := send("other@hosta")
	if res.OK || res.Reason != safesend.ReasonForbidden || res.Retryable {
		t.Errorf("ungranted send: %+v", res)
	}
	if len(h.backend.sent) != 1 {
		t.Errorf("backend saw %d sends, want 1", len(h.backend.sent))
	}
	// A grant for read doesn't allow send, and vice versa.
	if rec := h.post("status", `{"address":"mergentic-opencode@hostb"}`); rec.Code != 403 {
		t.Errorf("status via send grant: %d", rec.Code)
	}
}

func TestListFiltering(t *testing.T) {
	h := newHarness(t,
		gatewayapi.Grant{Ops: []string{"list"}, Sessions: []string{"a*@hostA"}},
		gatewayapi.Grant{Ops: []string{"send"}, Sessions: []string{"*@*"}}, // not list: must not widen
		gatewayapi.Grant{Ops: []string{"list", "read"}, Sessions: []string{"zed@hostA"}},
	)
	h.backend.sessions = []ops.Session{{Address: "alpha@hostA"}, {Address: "beta@hostA"}, {Address: "zed@hostA"}}
	rec := h.post("list", ``)
	if rec.Code != 200 {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	var resp gatewayapi.ListResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Host != "hostA" || len(resp.Sessions) != 2 || resp.Sessions[0].Address != "alpha@hostA" || resp.Sessions[1].Address != "zed@hostA" {
		t.Errorf("resp = %+v", resp)
	}

	h.id.Grants = []gatewayapi.Grant{all("read")}
	h.flush()
	if rec := h.post("list", `{}`); rec.Code != 403 {
		t.Errorf("list without list grant: %d", rec.Code)
	}

	h.id.Grants = []gatewayapi.Grant{{Ops: []string{"list"}, Sessions: []string{"nomatch@*"}}}
	h.flush()
	rec = h.post("list", `{}`)
	if !strings.Contains(rec.Body.String(), `"sessions":[]`) {
		t.Errorf("empty list should be [], got %s", rec.Body)
	}
}

func TestInvalidGrantsGrantNothing(t *testing.T) {
	h := newHarness(t, ValidGrants([]gatewayapi.Grant{
		{Ops: []string{"send", "teleport"}, Sessions: []string{"*@*"}},
		{Ops: []string{"send"}, Sessions: nil},
		{Ops: nil, Sessions: []string{"*@*"}},
	})...)
	if rec := h.post("list", `{}`); rec.Code != 403 {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestSendPrincipalAndLimits(t *testing.T) {
	h := newHarness(t, all("send"))
	// The request can't name the principal: By is an unknown field, and From
	// is only provenance.
	if rec := h.post("send", `{"address":"a@hostA","text":"t","via":"sent","by":"root"}`); rec.Code != 400 {
		t.Errorf("by field: %d %s", rec.Code, rec.Body)
	}
	rec := h.post("send", `{"address":"a@hostA#t1","text":"hello","via":"relayed","from":"me","correlation":"c1","wait_seconds":99999,"confirm_seconds":-5}`)
	if rec.Code != 200 {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}
	got := h.backend.sent[0]
	if got.By != "orch-box" || got.From != "me" || got.Via != "relayed" || got.Correlation != "c1" || got.Text != "hello" || got.Address != "a@hostA#t1" {
		t.Errorf("send request = %+v", got)
	}
	if got.Wait != maxWait || got.Confirm != defaultConfirm {
		t.Errorf("wait %v confirm %v", got.Wait, got.Confirm)
	}
	h.post("send", `{"address":"a@hostA","text":"t","via":"sent","wait_seconds":3,"confirm_seconds":999}`)
	if got := h.backend.sent[1]; got.Wait != 3*time.Second || got.Confirm != maxConfirm {
		t.Errorf("wait %v confirm %v", got.Wait, got.Confirm)
	}
}

func TestSendRefusalFromOpsIsPassedThrough(t *testing.T) {
	h := newHarness(t, all("send"))
	h.backend.sendRes = &ops.SendResult{Address: "a@hostA", Reason: safesend.ReasonBusy, Retryable: true, Detail: "mid-turn"}
	rec := h.post("send", `{"address":"a@hostA","text":"t","via":"sent"}`)
	var res ops.SendResult
	json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != 200 || res.OK || res.Reason != safesend.ReasonBusy || !res.Retryable {
		t.Errorf("%d %+v", rec.Code, res)
	}
}

func TestRateLimit(t *testing.T) {
	h := newHarness(t, all("send", "status"))
	send := Limit{PerMinute: 6, Burst: 2}
	other := Limit{PerMinute: 60, Burst: 3}
	h.srv = New(Config{
		Backend: h.backend, SendLimit: &send, OtherLimit: &other,
		Whois: func(context.Context, string) (Identity, error) { return h.id, nil },
		Now:   func() time.Time { return h.now },
	})
	sendBody := `{"address":"a@hostA","text":"t","via":"sent","correlation":"c9"}`
	for i := 0; i < 2; i++ {
		var res ops.SendResult
		json.Unmarshal(h.post("send", sendBody).Body.Bytes(), &res)
		if !res.OK {
			t.Fatalf("send %d refused: %+v", i, res)
		}
	}
	rec := h.post("send", sendBody)
	var res ops.SendResult
	json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != 200 || res.OK || res.Reason != safesend.ReasonRateLimited || !res.Retryable || res.Correlation != "c9" {
		t.Errorf("over-limit send: %d %+v", rec.Code, res)
	}
	if len(h.backend.sent) != 2 {
		t.Errorf("backend saw %d sends", len(h.backend.sent))
	}
	// Send and other buckets are separate.
	for i := 0; i < 3; i++ {
		if rec := h.post("status", `{"address":"a@hostA"}`); rec.Code != 200 {
			t.Fatalf("status %d: %d", i, rec.Code)
		}
	}
	rec = h.post("status", `{"address":"a@hostA"}`)
	if rec.Code != 429 || errReason(t, rec) != safesend.ReasonRateLimited || rec.Header().Get("Retry-After") != "1" {
		t.Errorf("over-limit status: %d %s %v", rec.Code, rec.Body, rec.Header())
	}
	// Per principal: another node isn't affected.
	h.id.Node = "other"
	h.flush()
	if rec := h.post("status", `{"address":"a@hostA"}`); rec.Code != 200 {
		t.Errorf("other principal: %d", rec.Code)
	}
	// Tokens come back over time.
	h.id.Node = "orch-box"
	h.flush()
	h.now = h.now.Add(2 * time.Second)
	if rec := h.post("status", `{"address":"a@hostA"}`); rec.Code != 200 {
		t.Errorf("after refill: %d", rec.Code)
	}
}

func TestBodyAndRouteErrors(t *testing.T) {
	h := newHarness(t, all("send", "status", "list", "events"))
	big := `{"address":"a@hostA","text":"` + strings.Repeat("x", MaxBodyBytes) + `","via":"sent"}`
	if rec := h.post("send", big); rec.Code != http.StatusRequestEntityTooLarge || errReason(t, rec) != safesend.ReasonInvalid {
		t.Errorf("big body: %d", rec.Code)
	}
	for name, body := range map[string]string{
		"unknown field": `{"address":"a@hostA","nope":1}`,
		"trailing":      `{"address":"a@hostA"}{}`,
		"not json":      `address`,
		"empty":         ``,
	} {
		if rec := h.post("status", body); rec.Code != 400 || errReason(t, rec) != safesend.ReasonInvalid {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := h.post("list", ``); rec.Code != 200 {
		t.Errorf("empty list body: %d", rec.Code)
	}
	if rec := h.post("list", `{"x":1}`); rec.Code != 400 {
		t.Errorf("list with field: %d", rec.Code)
	}
	if rec := h.do(http.MethodGet, "/v1/list", ""); rec.Code != 405 || rec.Header().Get("Allow") != "POST" {
		t.Errorf("GET: %d", rec.Code)
	}
	for _, p := range []string{"/", "/v1/", "/v1/nope", "/v2/list", "/v1/list/extra"} {
		if rec := h.do(http.MethodPost, p, `{}`); rec.Code != 404 {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	rec := h.post("events", `{}`)
	if rec.Code != 501 || errReason(t, rec) != safesend.ReasonUnsupported {
		t.Errorf("events: %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type %q", ct)
	}
}

func TestBackendErrorMapping(t *testing.T) {
	h := newHarness(t, all("status", "read", "threads", "list"))
	for reason, want := range map[safesend.Reason]int{
		safesend.ReasonNotFound:    404,
		safesend.ReasonInvalid:     400,
		safesend.ReasonNotLocal:    421,
		safesend.ReasonUnsupported: 501,
		safesend.ReasonFailed:      500,
	} {
		h.backend.err = ops.Refuse(reason, "d")
		for _, op := range []string{"status", "read", "threads"} {
			rec := h.post(op, `{"address":"a@hostA"}`)
			if rec.Code != want || errReason(t, rec) != reason {
				t.Errorf("%s %s: %d %s", op, reason, rec.Code, rec.Body)
			}
		}
		if rec := h.post("list", `{}`); rec.Code != want {
			t.Errorf("list %s: %d", reason, rec.Code)
		}
	}
	h.backend.err = errors.New("daemon down")
	if rec := h.post("status", `{"address":"a@hostA"}`); rec.Code != 500 || errReason(t, rec) != safesend.ReasonFailed {
		t.Errorf("plain error: %d", rec.Code)
	}
}

func TestWhoisCache(t *testing.T) {
	h := newHarness(t, all("status"))
	for i := 0; i < 3; i++ {
		h.post("status", `{"address":"a@hostA"}`)
	}
	if h.whoisN != 1 {
		t.Errorf("whois ran %d times within the ttl", h.whoisN)
	}
	h.now = h.now.Add(WhoisTTL + time.Second)
	h.post("status", `{"address":"a@hostA"}`)
	if h.whoisN != 2 {
		t.Errorf("whois ran %d times after the ttl", h.whoisN)
	}
	h.err = errors.New("down") // failures are not cached, and fail closed
	h.now = h.now.Add(WhoisTTL + time.Second)
	if rec := h.post("status", `{"address":"a@hostA"}`); rec.Code != 403 {
		t.Errorf("after expiry with whois down: %d", rec.Code)
	}
}

func TestAccessLogHasNoMessageText(t *testing.T) {
	h := newHarness(t, all("send"))
	h.post("send", `{"address":"a@hostA","text":"SECRET-BODY-TEXT","via":"sent"}`)
	h.post("send", `{"address":"a@hostA\n","text":"SECRET-BODY-TEXT","via":"sent"}`)
	logs := h.logs.String()
	if strings.Contains(logs, "SECRET") {
		t.Errorf("log leaks text:\n%s", logs)
	}
	for _, want := range []string{"principal=orch-box", "op=send", `target="a@hostA"`, "status=200", "result=ok", "took="} {
		if !strings.Contains(logs, want) {
			t.Errorf("log missing %q:\n%s", want, logs)
		}
	}
	if strings.Count(logs, "\n") != 2 {
		t.Errorf("want one line per request:\n%s", logs)
	}
}
