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
		{"t-ask", "ask"}, {"t-pending", "pending"}, {"t-answered", "answered"}, {"t-failed", "failed"}, {"t-proj", "mergentic"},
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
	thread, msg, err := asksClientFor(s.URL).PostAsk(context.Background(), "Launch X?", "Please decide @everyone <@888>", []string{"mergentic"})
	if err != nil || thread != "900" || msg != "500" {
		t.Fatalf("got %q %q %v", thread, msg, err)
	}
	p := f.hook[0]
	if !strings.HasPrefix(p["content"].(string), "<@777>\n") {
		t.Fatalf("content = %q", p["content"])
	}
	am := p["allowed_mentions"].(map[string]any)
	if len(am["parse"].([]any)) != 0 || len(am["users"].([]any)) != 1 || am["users"].([]any)[0] != "777" {
		t.Fatalf("allowed_mentions = %#v", am)
	}
	tags := p["applied_tags"].([]any)
	if len(tags) != 3 || tags[0] != "t-ask" || tags[1] != "t-pending" || tags[2] != "t-proj" {
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
	_, _, err := asksClientFor(s.URL).PostAsk(context.Background(), "T", "b", []string{"nope"})
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
	if !strings.HasPrefix(f.hook[1]["content"].(string), "<@777>") {
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
	if err := c.CloseAsk(context.Background(), "901", ""); err == nil || f.patched != nil {
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
	s := f.server(t, map[string]Channel{"900": {ID: "900", ParentID: "forum", AppliedTags: []string{"t-ask", "t-pending", "t-proj"}}}, nil)
	defer s.Close()
	if err := asksClientFor(s.URL).CloseAsk(context.Background(), "900", "failed"); err != nil {
		t.Fatal(err)
	}
	tags := f.patched["applied_tags"].([]any)
	if len(tags) != 3 || tags[0] != "t-ask" || tags[1] != "t-proj" || tags[2] != "t-failed" {
		t.Fatalf("tags = %#v", tags)
	}
	if f.patched["archived"] != true || f.patched["locked"] != true {
		t.Fatalf("patched = %#v", f.patched)
	}
	if err := asksClientFor(s.URL).CloseAsk(context.Background(), "900", "bogus"); err == nil {
		t.Fatal("unknown outcome accepted")
	}
}

func TestCollabSkipsAskThreads(t *testing.T) {
	var s *httptest.Server
	s = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/channels/forum":
			writeJSON(t, w, Channel{ID: "forum", GuildID: "g", AvailableTags: []ForumTag{{"t-ask", "ask"}}})
		case r.URL.Path == "/api/guilds/g/threads/active":
			writeJSON(t, w, threadList{Threads: []Channel{
				{ID: "1", ParentID: "forum", Name: "[proj] normal"},
				{ID: "2", ParentID: "forum", Name: "[proj] an ask", AppliedTags: []string{"t-ask"}},
			}})
		case strings.HasSuffix(r.URL.Path, "/threads/archived/public"):
			writeJSON(t, w, threadList{})
		case r.URL.Path == "/api/channels/2":
			writeJSON(t, w, Channel{ID: "2", ParentID: "forum", Name: "[proj] an ask", AppliedTags: []string{"t-ask"}})
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
		t.Fatal("ask thread readable via collab read")
	}
}
