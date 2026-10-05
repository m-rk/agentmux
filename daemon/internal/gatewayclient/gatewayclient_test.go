package gatewayclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/gatewayapi"
	"github.com/m-rk/agentmux/daemon/internal/ops"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
	"github.com/m-rk/agentmux/daemon/internal/transcript"
)

// stub serves one reply and records the request it got.
type stub struct {
	status int
	body   string
	path   string
	method string
	ctype  string
	req    []byte
}

func (s *stub) server(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.path, s.method, s.ctype = r.URL.Path, r.Method, r.Header.Get("Content-Type")
		s.req, _ = io.ReadAll(r.Body)
		w.WriteHeader(s.status)
		io.WriteString(w, s.body)
	}))
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, HTTP: srv.Client(), Host: "box"}
}

func TestSuccess(t *testing.T) {
	ctx := context.Background()

	s := &stub{status: 200, body: `{"host":"box","sessions":[{"address":"a@box","name":"a","agent":"claude-code","status":"idle","workdir":"/w"}]}`}
	list, err := s.server(t).List(ctx)
	if err != nil || list.Host != "box" || len(list.Sessions) != 1 || s.path != "/v1/list" || s.method != "POST" || s.ctype != "application/json" {
		t.Fatalf("list: %+v %v (%s %s %s)", list, err, s.method, s.path, s.ctype)
	}

	s = &stub{status: 200, body: `{"address":"a@box","name":"a","agent":"amp","status":"idle","workdir":"/w","state":"ready","thread":"T-1"}`}
	st, err := s.server(t).Status(ctx, gatewayapi.StatusRequest{Address: "a@box#T-1"})
	if err != nil || st.State != "ready" || st.Thread != "T-1" || s.path != "/v1/status" || string(s.req) != `{"address":"a@box#T-1"}` {
		t.Fatalf("status: %+v %v %s %s", st, err, s.path, s.req)
	}

	s = &stub{status: 200, body: `{"threads":[{"id":"T-1","title":"x","updated":"2026-01-02T03:04:05Z"}]}`}
	th, err := s.server(t).Threads(ctx, gatewayapi.ThreadsRequest{Address: "a@box"})
	if err != nil || len(th.Threads) != 1 || th.Threads[0].ID != "T-1" || s.path != "/v1/threads" {
		t.Fatalf("threads: %+v %v", th, err)
	}

	s = &stub{status: 200, body: `{"thread":"T-1","messages":[{"role":"user","text":"hi"}],"older":"c2"}`}
	page, err := s.server(t).Read(ctx, gatewayapi.ReadRequest{Address: "a@box", Cursor: "c1", Limit: 5})
	if err != nil || page.Older != "c2" || len(page.Messages) != 1 || s.path != "/v1/read" {
		t.Fatalf("read: %+v %v", page, err)
	}
	var rq map[string]any
	json.Unmarshal(s.req, &rq)
	if rq["address"] != "a@box" || rq["cursor"] != "c1" || rq["limit"] != float64(5) {
		t.Fatalf("read request: %s", s.req)
	}
	var _ transcript.Page = page
}

func TestRetireAndGCPaths(t *testing.T) {
	ctx := context.Background()

	s := &stub{status: 200, body: `{"address":"task-1@box","name":"task-1","agent":"amp","status":"idle","workdir":"/w","state":"ready"}`}
	res, err := s.server(t).Retire(ctx, gatewayapi.RetireRequest{Address: "task-1@box"})
	if err != nil || res.Address != "task-1@box" || s.path != "/v1/retire" || s.method != "POST" {
		t.Fatalf("retire: %+v %v (%s %s)", res, err, s.method, s.path)
	}
	var rq map[string]any
	json.Unmarshal(s.req, &rq)
	if rq["address"] != "task-1@box" {
		t.Fatalf("retire request: %s", s.req)
	}

	s = &stub{status: 200, body: `{"retention_days":14,"deleted":[{"instance":"task-1","agent":"amp"}],"kept":[]}`}
	gc, err := s.server(t).GC(ctx, gatewayapi.GCRequest{DryRun: true})
	if err != nil || gc.RetentionDays != 14 || len(gc.Deleted) != 1 || s.path != "/v1/gc" {
		t.Fatalf("gc: %+v %v %s", gc, err, s.path)
	}
}

func TestErrorResponse(t *testing.T) {
	s := &stub{status: 404, body: `{"error":{"reason":"not_found","detail":"no instance \"a\" on this host"}}`}
	_, err := s.server(t).Status(context.Background(), gatewayapi.StatusRequest{Address: "a@box"})
	e, ok := err.(*ops.Error)
	if !ok || e.Reason != safesend.ReasonNotFound || !strings.Contains(e.Detail, "no instance") {
		t.Fatalf("got %#v", err)
	}
	s = &stub{status: 403, body: `{"error":{"reason":"forbidden","detail":"nope"}}`}
	_, err = s.server(t).Threads(context.Background(), gatewayapi.ThreadsRequest{Address: "a@box"})
	if e := ops.AsError(err); e.Reason != safesend.ReasonForbidden || e.Detail != "nope" {
		t.Fatalf("got %#v", err)
	}
}

func TestUnparseableErrorBody(t *testing.T) {
	for _, body := range []string{"<html>bad gateway</html>", "", `{"error":{}}`} {
		s := &stub{status: 502, body: body}
		_, err := s.server(t).List(context.Background())
		e := ops.AsError(err)
		if e.Reason != safesend.ReasonFailed || !strings.Contains(e.Detail, "HTTP 502") {
			t.Errorf("body %q: got %#v", body, err)
		}
	}
}

func TestBadSuccessBody(t *testing.T) {
	s := &stub{status: 200, body: `not json`}
	_, err := s.server(t).List(context.Background())
	if e := ops.AsError(err); e.Reason != safesend.ReasonFailed || !strings.Contains(e.Detail, "bad list reply") {
		t.Fatalf("got %#v", err)
	}
}

func TestNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c := &Client{BaseURL: url, Host: "box"}
	_, err := c.List(context.Background())
	e := ops.AsError(err)
	if e.Reason != safesend.ReasonFailed || !strings.Contains(e.Detail, `"box"`) || !strings.Contains(e.Detail, "unreachable") {
		t.Fatalf("got %#v", err)
	}
}

func TestResponseCap(t *testing.T) {
	s := &stub{status: 200, body: `{"host":"` + strings.Repeat("x", MaxResponseBytes) + `"}`}
	_, err := s.server(t).List(context.Background())
	if e := ops.AsError(err); e.Reason != safesend.ReasonFailed || !strings.Contains(e.Detail, "over") {
		t.Fatalf("got %#v", err)
	}
}

func TestContextCancelled(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	defer srv.Close()
	defer close(block)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	c := &Client{BaseURL: srv.URL, Host: "box"}
	if _, err := c.List(ctx); ops.AsError(err).Reason != safesend.ReasonFailed {
		t.Fatalf("got %#v", err)
	}
}

func TestSendSuccessAndRequest(t *testing.T) {
	s := &stub{status: 200, body: `{"ok":true,"address":"a@box","agent":"claude-code","confirmed":true,"bytes":9,"correlation":"c1"}`}
	res := s.server(t).Send(context.Background(), gatewayapi.SendRequest{
		Address: "a@box", Text: "hello", Via: "relayed", From: "task-1", Correlation: "c1", WaitSeconds: 3, ConfirmSeconds: 15,
	})
	if !res.OK || !res.Confirmed || res.Agent != "claude-code" || s.path != "/v1/send" {
		t.Fatalf("got %+v (%s)", res, s.path)
	}
	var rq map[string]any
	if err := json.Unmarshal(s.req, &rq); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"address": "a@box", "text": "hello", "via": "relayed", "from": "task-1", "correlation": "c1", "wait_seconds": 3.0, "confirm_seconds": 15.0}
	if len(rq) != len(want) {
		t.Fatalf("request fields: %s", s.req)
	}
	for k, v := range want {
		if rq[k] != v {
			t.Errorf("%s = %v, want %v", k, rq[k], v)
		}
	}
	for _, k := range []string{"by", "By", "principal"} {
		if _, ok := rq[k]; ok {
			t.Errorf("send request carries %q", k)
		}
	}
}

func TestSendRefusalPassthrough(t *testing.T) {
	s := &stub{status: 200, body: `{"ok":false,"address":"a@box","reason":"busy","retryable":true,"detail":"mid-turn","confirmed":false,"correlation":"c1"}`}
	res := s.server(t).Send(context.Background(), gatewayapi.SendRequest{Address: "a@box", Text: "x", Via: "relayed", Correlation: "c1"})
	if res.OK || res.Reason != safesend.ReasonBusy || !res.Retryable || res.Detail != "mid-turn" || res.Correlation != "c1" {
		t.Fatalf("got %+v", res)
	}
}

func TestSendErrorsBecomeRefusals(t *testing.T) {
	s := &stub{status: 403, body: `{"error":{"reason":"forbidden","detail":"no send grant"}}`}
	res := s.server(t).Send(context.Background(), gatewayapi.SendRequest{Address: "a@box", Text: "x", Via: "relayed", Correlation: "c1"})
	if res.OK || res.Reason != safesend.ReasonForbidden || res.Detail != "no send grant" || res.Address != "a@box" || res.Correlation != "c1" {
		t.Fatalf("got %+v", res)
	}
	s = &stub{status: 429, body: `{"error":{"reason":"rate_limited","detail":"slow down"}}`}
	if res := s.server(t).Send(context.Background(), gatewayapi.SendRequest{Address: "a@box"}); res.Reason != safesend.ReasonRateLimited || !res.Retryable {
		t.Fatalf("got %+v", res)
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	res = (&Client{BaseURL: url, Host: "box"}).Send(context.Background(), gatewayapi.SendRequest{Address: "a@box"})
	if res.OK || res.Reason != safesend.ReasonFailed || !strings.Contains(res.Detail, `"box"`) {
		t.Fatalf("got %+v", res)
	}
}

func TestBaseURLTrailingSlash(t *testing.T) {
	s := &stub{status: 200, body: `{}`}
	c := s.server(t)
	c.BaseURL += "/"
	if _, err := c.List(context.Background()); err != nil || s.path != "/v1/list" {
		t.Fatalf("%v %q", err, s.path)
	}
}

func TestCreate(t *testing.T) {
	ctx := context.Background()
	s := &stub{status: 200, body: `{"address":"t@box","name":"t","agent":"opencode","status":"running","workdir":"/w/t","branch":"feature/x","created":true}`}
	got, err := s.server(t).Create(ctx, gatewayapi.CreateRequest{Template: "tmpl@box", Instance: "t", Branch: "feature/x", AllowFiles: []string{"/n/a.md"}})
	if err != nil || got.Address != "t@box" || got.Workdir != "/w/t" || got.Branch != "feature/x" || !got.Created || s.path != "/v1/create" {
		t.Fatalf("create: %+v %v %s", got, err, s.path)
	}
	if string(s.req) != `{"template":"tmpl@box","instance":"t","branch":"feature/x","allow_files":["/n/a.md"]}` {
		t.Errorf("request = %s", s.req)
	}

	s = &stub{status: 403, body: `{"error":{"reason":"forbidden","detail":"nope"}}`}
	_, err = s.server(t).Create(ctx, gatewayapi.CreateRequest{Instance: "t"})
	if e := ops.AsError(err); e.Reason != safesend.ReasonForbidden {
		t.Fatalf("refusal: %v", err)
	}
}

func TestRun(t *testing.T) {
	ctx := context.Background()
	s := &stub{status: 200, body: `{"ok":true,"address":"probe@box#T-11111111-1111-4111-8111-111111111111","agent":"amp","thread":"T-11111111-1111-4111-8111-111111111111","thread_id":"T-11111111-1111-4111-8111-111111111111","thread_url":"https://ampcode.com/threads/T-11111111-1111-4111-8111-111111111111","state":"running"}`}
	got, err := s.server(t).Run(ctx, gatewayapi.RunRequest{Address: "probe@box", Text: "do the thing", Title: "AMUX-17 do the thing"})
	if err != nil || got.ThreadID != "T-11111111-1111-4111-8111-111111111111" || got.State != "running" || s.path != "/v1/run" {
		t.Fatalf("run: %+v %v %s", got, err, s.path)
	}
	if string(s.req) != `{"address":"probe@box","text":"do the thing","title":"AMUX-17 do the thing"}` {
		t.Errorf("request = %s", s.req)
	}

	s = &stub{status: 403, body: `{"error":{"reason":"forbidden","detail":"nope"}}`}
	_, err = s.server(t).Run(ctx, gatewayapi.RunRequest{Address: "probe@box"})
	if e := ops.AsError(err); e.Reason != safesend.ReasonForbidden {
		t.Fatalf("refusal: %v", err)
	}
}
