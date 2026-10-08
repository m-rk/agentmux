package collab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type fakeAsks struct {
	mu      sync.Mutex
	hook    []map[string]any
	hookQ   []string
	patched map[string]any
}

func (f *fakeAsks) server(t *testing.T, threads map[string]Channel, messages []Message) *httptest.Server {
	forum := Channel{ID: "forum", GuildID: "guild", Type: 15, AvailableTags: []ForumTag{
		{"t-task", "task"}, {"t-epic", "epic"}, {"t-idea", "idea"}, {"t-spike", "spike"},
		{"t-needsme", "needs me"}, {"t-working", "working"}, {"t-blocked", "blocked"},
		{"t-parked", "parked"}, {"t-notnow", "not now"}, {"t-done", "done"}, {"t-failed", "failed"},
		{"t-ask", "ask"}, {"t-pending", "pending"}, {"t-answered", "answered"}, {"t-launched", "launched"},
		{"t-proj", "mergentic"},
	}}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/api/channels/forum":
			writeJSON(t, w, forum)
		case r.URL.Path == "/hook" && r.Method == http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.hook = append(f.hook, body)
			f.hookQ = append(f.hookQ, r.URL.RawQuery)
			writeJSON(t, w, Message{ID: "500", ChannelID: "900"})
		case strings.HasSuffix(r.URL.Path, "/messages"):
			writeJSON(t, w, messages)
		case strings.HasPrefix(r.URL.Path, "/api/channels/") && r.Method == http.MethodPatch:
			_ = json.NewDecoder(r.Body).Decode(&f.patched)
			w.WriteHeader(200)
			_, _ = w.Write([]byte("{}"))
		case strings.HasPrefix(r.URL.Path, "/api/channels/"):
			id := strings.TrimPrefix(r.URL.Path, "/api/channels/")
			if th, ok := threads[id]; ok {
				writeJSON(t, w, th)
				return
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
}

func asksClientFor(url string) *Client {
	c := testClient(url)
	c.Config.AskMentionUserID = "777"
	return c
}

func TestPostAskMentionsOnlyConfiguredUser(t *testing.T) {
	f := &fakeAsks{}
	s := f.server(t, nil, nil)
	defer s.Close()
	thread, msg, err := asksClientFor(s.URL).PostAsk(context.Background(), "Launch X?", "Please decide @everyone <@888>", []string{"mergentic"}, AskOptions{})
	if err != nil || thread != "900" || msg != "500" {
		t.Fatalf("got %q %q %v", thread, msg, err)
	}
	p := f.hook[0]
	if p["content"] != "<@777> Please decide @everyone <@888>" {
		t.Fatalf("content = %q", p["content"])
	}
	am := p["allowed_mentions"].(map[string]any)
	if len(am["parse"].([]any)) != 0 || len(am["users"].([]any)) != 1 || am["users"].([]any)[0] != "777" {
		t.Fatalf("allowed_mentions = %#v", am)
	}
	tags := p["applied_tags"].([]any)
	if len(tags) != 3 || tags[0] != "t-task" || tags[1] != "t-proj" || tags[2] != "t-needsme" {
		t.Fatalf("tags = %#v", tags)
	}
	if p["thread_name"] != "Launch X?" {
		t.Fatalf("thread_name = %v", p["thread_name"])
	}
}

func TestPostAskMissingTag(t *testing.T) {
	f := &fakeAsks{}
	s := f.server(t, nil, nil)
	defer s.Close()
	_, _, err := asksClientFor(s.URL).PostAsk(context.Background(), "T", "b", []string{"nope"}, AskOptions{})
	if err == nil || !strings.Contains(err.Error(), `"nope"`) || len(f.hook) != 0 {
		t.Fatalf("err = %v, posts = %d", err, len(f.hook))
	}
}

func TestReplyAskMention(t *testing.T) {
	f := &fakeAsks{}
	s := f.server(t, map[string]Channel{"900": {ID: "900", ParentID: "forum", AppliedTags: []string{"t-ask"}}}, nil)
	defer s.Close()
	c := asksClientFor(s.URL)
	if _, err := c.ReplyAsk(context.Background(), "900", "plain", false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReplyAsk(context.Background(), "900", "ping", true); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.hook[0]["allowed_mentions"].(map[string]any)["users"]; ok || strings.Contains(f.hookQ[0], "thread_id=900") == false {
		t.Fatalf("plain reply = %#v %s", f.hook[0], f.hookQ[0])
	}
	if f.hook[1]["content"] != "<@777> ping" {
		t.Fatalf("mention reply = %#v", f.hook[1])
	}
}

func TestAsksRefuseNonAskThread(t *testing.T) {
	f := &fakeAsks{}
	s := f.server(t, map[string]Channel{"901": {ID: "901", ParentID: "forum"}}, nil)
	defer s.Close()
	c := asksClientFor(s.URL)
	if _, err := c.ReplyAsk(context.Background(), "901", "x", false); err == nil {
		t.Fatal("reply to non-ask thread allowed")
	}
	if err := c.CloseAsk(context.Background(), "901", "", false); err == nil || f.patched != nil {
		t.Fatal("close of non-ask thread allowed")
	}
}

func TestReadAskFlagsConfiguredUser(t *testing.T) {
	f := &fakeAsks{}
	msgs := []Message{
		{ID: "12", Content: "yes", Author: Author{ID: "777", Username: "mark"}},
		{ID: "10", Content: "opening", WebhookID: "wh", Author: Author{ID: "wh", Username: "agentmux asks"}},
		{ID: "11", Content: "spoof", Author: Author{ID: "778", Username: "mark"}},
	}
	s := f.server(t, map[string]Channel{"900": {ID: "900", ParentID: "forum", AppliedTags: []string{"t-ask"}}}, msgs)
	defer s.Close()
	got, err := asksClientFor(s.URL).ReadAsk(context.Background(), "900", "")
	if err != nil || len(got) != 3 {
		t.Fatalf("got %v %v", got, err)
	}
	if got[0].ID != "10" || got[0].AuthorIsConfiguredUser || got[1].AuthorIsConfiguredUser || !got[2].AuthorIsConfiguredUser || got[2].Text != "yes" {
		t.Fatalf("messages = %#v", got)
	}
}

func TestCloseAskSwapsOutcomeTagAndArchives(t *testing.T) {
	f := &fakeAsks{}
	s := f.server(t, map[string]Channel{"900": {ID: "900", ParentID: "forum", AppliedTags: []string{"t-task", "t-pending", "t-proj"}}}, nil)
	defer s.Close()
	if err := asksClientFor(s.URL).CloseAsk(context.Background(), "900", "failed", true); err != nil {
		t.Fatal(err)
	}
	tags := f.patched["applied_tags"].([]any)
	if len(tags) != 3 || tags[0] != "t-task" || tags[1] != "t-proj" || tags[2] != "t-failed" {
		t.Fatalf("tags = %#v", tags)
	}
	if f.patched["archived"] != true || f.patched["locked"] != true {
		t.Fatalf("patched = %#v", f.patched)
	}
	if err := asksClientFor(s.URL).CloseAsk(context.Background(), "900", "bogus", false); err == nil {
		t.Fatal("unknown outcome accepted")
	}
	if err := asksClientFor(s.URL).CloseAsk(context.Background(), "900", "pending", false); err == nil {
		t.Fatal("retired outcome accepted")
	}
}

func TestCloseAskWithoutLockLeavesThreadReopenable(t *testing.T) {
	f := &fakeAsks{}
	s := f.server(t, map[string]Channel{"900": {ID: "900", ParentID: "forum", AppliedTags: []string{"t-task", "t-working"}}}, nil)
	defer s.Close()
	if err := asksClientFor(s.URL).CloseAsk(context.Background(), "900", "", false); err != nil {
		t.Fatal(err)
	}
	if f.patched["archived"] != true || f.patched["locked"] != false {
		t.Fatalf("patched = %#v", f.patched)
	}
}

func TestPostAskInThreadReopensAndMentions(t *testing.T) {
	f := &fakeAsks{}
	s := f.server(t, map[string]Channel{"900": {ID: "900", ParentID: "forum", AppliedTags: []string{"t-task", "t-answered", "t-proj"}}}, nil)
	defer s.Close()
	id, err := asksClientFor(s.URL).PostAskInThread(context.Background(), "900", "MERG-4 combine tasks", "Context first.\n\nnext question @everyone", nil, AskOptions{})
	if err != nil || id != "500" {
		t.Fatalf("got %q %v", id, err)
	}
	tags := f.patched["applied_tags"].([]any)
	if len(tags) != 3 || tags[0] != "t-task" || tags[1] != "t-proj" || tags[2] != "t-needsme" {
		t.Fatalf("tags = %#v", tags)
	}
	if f.patched["archived"] != false || f.patched["auto_archive_duration"] != float64(10080) || f.patched["name"] != "MERG-4 combine tasks" {
		t.Fatalf("patched = %#v", f.patched)
	}
	if len(f.hook) != 1 || !strings.Contains(f.hookQ[0], "thread_id=900") || f.hook[0]["content"] != "Context first.\n\n<@777> next question @everyone" {
		t.Fatalf("hook = %#v %v", f.hook, f.hookQ)
	}
	am := f.hook[0]["allowed_mentions"].(map[string]any)
	if len(am["users"].([]any)) != 1 || am["users"].([]any)[0] != "777" {
		t.Fatalf("allowed_mentions = %#v", am)
	}
	if _, ok := f.hook[0]["thread_name"]; ok {
		t.Fatal("thread_name would create a new post")
	}
}

func TestPostAskInThreadNoTitleKeepsName(t *testing.T) {
	f := &fakeAsks{}
	s := f.server(t, map[string]Channel{"900": {ID: "900", ParentID: "forum", AppliedTags: []string{"t-ask"}}}, nil)
	defer s.Close()
	if _, err := asksClientFor(s.URL).PostAskInThread(context.Background(), "900", "", "q", nil, AskOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.patched["name"]; ok {
		t.Fatalf("patched = %#v", f.patched)
	}
}

func TestPostAskInThreadRefusesNonAskThread(t *testing.T) {
	f := &fakeAsks{}
	s := f.server(t, map[string]Channel{"901": {ID: "901", ParentID: "forum"}}, nil)
	defer s.Close()
	if _, err := asksClientFor(s.URL).PostAskInThread(context.Background(), "901", "", "q", nil, AskOptions{}); err == nil || f.patched != nil || len(f.hook) != 0 {
		t.Fatalf("err = %v", err)
	}
}

func TestReadAskAfterPassesCursor(t *testing.T) {
	var after string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/channels/forum":
			writeJSON(t, w, Channel{ID: "forum", AvailableTags: []ForumTag{
				{"t-task", "task"}, {"t-ask", "ask"},
			}})
		case strings.HasSuffix(r.URL.Path, "/messages"):
			after = r.URL.Query().Get("after")
			writeJSON(t, w, []Message{})
		case r.URL.Path == "/api/channels/900":
			writeJSON(t, w, Channel{ID: "900", ParentID: "forum", AppliedTags: []string{"t-ask"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer s.Close()
	if _, err := asksClientFor(s.URL).ReadAsk(context.Background(), "900", "123"); err != nil || after != "123" {
		t.Fatalf("after = %q err = %v", after, err)
	}
}

func TestCollabSkipsAskThreads(t *testing.T) {
	var s *httptest.Server
	s = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/channels/forum":
			writeJSON(t, w, Channel{ID: "forum", GuildID: "g", AvailableTags: []ForumTag{
				{"t-task", "task"}, {"t-ask", "ask"},
			}})
		case r.URL.Path == "/api/guilds/g/threads/active":
			writeJSON(t, w, threadList{Threads: []Channel{
				{ID: "1", ParentID: "forum", Name: "[proj] normal"},
				{ID: "2", ParentID: "forum", Name: "AMUX-35 working", AppliedTags: []string{"t-task", "t-working"}},
				{ID: "3", ParentID: "forum", Name: "[proj] legacy ask", AppliedTags: []string{"t-ask"}},
			}})
		case strings.HasSuffix(r.URL.Path, "/threads/archived/public"):
			writeJSON(t, w, threadList{})
		case r.URL.Path == "/api/channels/2":
			writeJSON(t, w, Channel{ID: "2", ParentID: "forum", Name: "[proj] an ask", AppliedTags: []string{"t-task", "t-working"}})
		case r.URL.Path == "/api/channels/3":
			writeJSON(t, w, Channel{ID: "3", ParentID: "forum", Name: "[proj] legacy ask", AppliedTags: []string{"t-ask"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer s.Close()
	c := testClient(s.URL)
	threads, err := c.ListRelevantThreads(context.Background(), "proj")
	if err != nil || len(threads) != 1 || threads[0].ID != "1" {
		t.Fatalf("threads = %v %v", threads, err)
	}
	if _, err := c.RelevantThread(context.Background(), "2", "proj"); err == nil {
		t.Fatal("task thread readable via collab read")
	}
	if _, err := c.RelevantThread(context.Background(), "3", "proj"); err == nil {
		t.Fatal("legacy ask thread readable via collab read")
	}
}
