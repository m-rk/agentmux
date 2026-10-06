package collab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// testThreadServer is a fake Discord that serves the asks forum, one
// thread (the "test thread", id 4242), thread messages, and records every
// bot write: posts, patches, reaction PUTs and deletes. The webhook is
// absent on purpose: test sends must never touch it.
type testThreadServer struct {
	mu       sync.Mutex
	posts    []map[string]any // message POST bodies, in order
	postPath []string
	patches  []map[string]any
	puts     []string // reaction PUT paths
	deletes  []string // message DELETE paths
	reads    []string // message-list GET paths

	botUser   string
	messages  []Message // thread messages, newest first (Discord order)
	callbacks []map[string]any
}

func (s *testThreadServer) server(t *testing.T) *httptest.Server {
	t.Helper()
	forum := Channel{ID: "forum", GuildID: "guild", Type: 15, AvailableTags: []ForumTag{
		{"t-task", "task"}, {"t-needsme", "needs me"},
	}}
	if s.botUser == "" {
		s.botUser = "bot-1"
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		path := r.URL.EscapedPath()
		switch {
		case path == "/api/users/@me":
			writeJSON(t, w, map[string]string{"id": s.botUser})
		case path == "/api/channels/forum":
			writeJSON(t, w, forum)
		case path == "/api/guilds/guild/emojis":
			writeJSON(t, w, []GuildEmoji{{ID: "emoji-amp-id", Name: "amp"}})
		case r.Method == http.MethodPost && path == "/api/channels/4242/messages":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.posts, s.postPath = append(s.posts, body), append(s.postPath, path)
			writeJSON(t, w, Message{ID: "501", ChannelID: "4242"})
		case r.Method == http.MethodPatch && strings.Contains(path, "/messages/"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.patches = append(s.patches, body)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPut && strings.Contains(path, "/reactions/"):
			s.puts = append(s.puts, path)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && strings.Contains(path, "/messages/"):
			s.deletes = append(s.deletes, path)
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(path, "/api/interactions/"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.callbacks = append(s.callbacks, body)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/messages"):
			s.reads = append(s.reads, r.URL.RequestURI())
			msgs := s.messages
			if before := r.URL.Query().Get("before"); before != "" {
				kept := msgs[:0:0]
				for _, m := range msgs {
					if m.ID < before {
						kept = append(kept, m)
					}
				}
				msgs = kept
			}
			if msgs == nil {
				msgs = []Message{}
			}
			writeJSON(t, w, msgs)
		case r.Method == http.MethodGet && strings.Contains(path, "/messages/"):
			writeJSON(t, w, map[string]any{"id": "501", "content": "pick one", "components": []any{
				map[string]any{"type": 1, "components": []any{
					map[string]any{"type": 2, "style": 2, "label": "Ship it", "custom_id": "ask:Ship it"},
					map[string]any{"type": 2, "style": 2, "label": "Not now", "custom_id": "ask:Not now"},
				}},
			}})
		case path == "/api/channels/4242":
			writeJSON(t, w, Channel{ID: "4242", ParentID: "forum", AppliedTags: []string{"t-task"}})
		default:
			http.NotFound(w, r)
		}
	}))
}

func testThreadClient(url string) *Client {
	c := testClient(url)
	c.Config.AskMentionUserID = "777"
	c.Config.TestThreadID = "4242"
	return c
}

// TestTestPostIsABotReplyWithNoForumPost pins the AMUX-39 routing: a task
// session's post becomes one bot message in the test thread — no forum
// post, no webhook, no tags, no mention.
func TestTestPostIsABotReplyWithNoForumPost(t *testing.T) {
	s := &testThreadServer{}
	srv := s.server(t)
	defer srv.Close()
	msg, err := testThreadClient(srv.URL).PostTestMessage(context.Background(), "AMUX-39", "hello <@888> @everyone and :amp:?", AskOptions{Buttons: []AskButton{{Label: "Ship it", Emoji: ":amp:"}}})
	if err != nil || msg != "501" {
		t.Fatalf("got %q %v", msg, err)
	}
	if len(s.postPath) != 1 || s.postPath[0] != "/api/channels/4242/messages" {
		t.Fatalf("posts = %v", s.postPath)
	}
	p := s.posts[0]
	if p["content"] != "[AMUX-39] hello   and :amp:?" {
		t.Fatalf("content = %q", p["content"])
	}
	am := p["allowed_mentions"].(map[string]any)
	if len(am["parse"].([]any)) != 0 {
		t.Fatalf("allowed_mentions = %#v", am)
	}
	if _, ok := p["applied_tags"]; ok {
		t.Fatalf("test post sets tags: %#v", p)
	}
	if _, ok := p["username"]; ok {
		t.Fatalf("test post uses the webhook identity: %#v", p)
	}
	rows := p["components"].([]any)[0].(map[string]any)["components"].([]any)
	btn := rows[0].(map[string]any)
	if btn["custom_id"] != "ask:Ship it" || btn["emoji"].(map[string]any)["id"] != "emoji-amp-id" {
		t.Fatalf("button = %#v", btn)
	}
}

// TestTestPostStripsMentions pins that every mention form is scrubbed and
// stays inert even pasted into a test body.
func TestTestPostStripsMentions(t *testing.T) {
	for _, body := range []string{
		"ping <@777> done", "nick <@!777> done", "role <@&123> done",
		"all @everyone here", "here @here now",
	} {
		s := &testThreadServer{}
		srv := s.server(t)
		if _, err := testThreadClient(srv.URL).PostTestMessage(context.Background(), "", body, AskOptions{}); err != nil {
			srv.Close()
			t.Fatalf("%q: %v", body, err)
		}
		srv.Close()
		got := s.posts[0]["content"].(string)
		if strings.Contains(got, "<@") || strings.Contains(got, "@everyone") || strings.Contains(got, "@here") {
			t.Fatalf("%q survived as %q", body, got)
		}
	}
}

// TestTestEditAfterPost pins the two-identities gotcha: posting then
// editing the same message goes through one identity (the bot), so the
// edit succeeds on what the post created.
func TestTestEditAfterPost(t *testing.T) {
	s := &testThreadServer{}
	srv := s.server(t)
	defer srv.Close()
	c := testThreadClient(srv.URL)
	msg, err := c.PostTestMessage(context.Background(), "AMUX-39", "pick one",
		AskOptions{Buttons: []AskButton{{Label: "Ship it"}, {Label: "Not now"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.EditTestMessage(context.Background(), msg, EditAskOptions{Body: "edited <@777>", Chosen: "Ship it"}); err != nil {
		t.Fatal(err)
	}
	if len(s.patches) != 1 {
		t.Fatalf("patches = %d", len(s.patches))
	}
	p := s.patches[0]
	if p["content"] != "edited" {
		t.Fatalf("content = %q", p["content"])
	}
	rows := p["components"].([]any)[0].(map[string]any)["components"].([]any)
	chosen := rows[0].(map[string]any)
	if chosen["style"] != float64(3) || chosen["label"] != "Ship it ✓" {
		t.Fatalf("chosen = %#v", chosen)
	}
}

// TestTestReactStaysInThread pins that a task session's react targets the
// test thread, never the passed thread.
func TestTestReactStaysInThread(t *testing.T) {
	s := &testThreadServer{}
	srv := s.server(t)
	defer srv.Close()
	if err := testThreadClient(srv.URL).ReactTestMessage(context.Background(), "501", "🤖"); err != nil {
		t.Fatal(err)
	}
	if len(s.puts) != 1 || !strings.Contains(s.puts[0], "/api/channels/4242/messages/501/reactions/") {
		t.Fatalf("puts = %v", s.puts)
	}
}

// TestTestCloseAndTagAreNoOps is client-side documentation: there is no
// client call for a task session to close or tag with — the CLI prints
// what it would do without touching Discord. What the client must
// guarantee instead is that the test thread id validates and anything
// else is distinguishable from it.
func TestTestThreadIdentity(t *testing.T) {
	c := testThreadClient("http://127.0.0.1:1")
	if !c.IsTestThread("4242") || c.IsTestThread("900") {
		t.Fatal("IsTestThread misidentifies threads")
	}
	if _, err := c.TestThreadID(); err != nil {
		t.Fatal(err)
	}
	plain := testClient("http://127.0.0.1:1")
	if _, err := plain.TestThreadID(); err == nil {
		t.Fatal("unconfigured TestThreadID succeeds: task sessions would post instead of refuse")
	}
}

// TestTestButtonClickNeverAnswers pins the serve side: a click on a test
// button is answered ephemerally and recorded nowhere.
func TestTestButtonClickNeverAnswers(t *testing.T) {
	s := &testThreadServer{}
	srv := s.server(t)
	defer srv.Close()
	l := &Listener{Client: testThreadClient(srv.URL), Store: &ClickStore{Path: t.TempDir() + "/clicks.jsonl"}}
	raw, err := json.Marshal(map[string]any{
		"id": "i1", "token": "tok", "type": 3, "channel_id": "4242",
		"channel": map[string]any{"parent_id": "forum"},
		"member":  map[string]any{"user": map[string]any{"id": "777"}},
		"data":    map[string]any{"custom_id": "ask:Ship it"},
		"message": map[string]any{"id": "501", "components": []any{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.HandleInteraction(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	clicks, _ := l.Store.load()
	if len(clicks) != 0 {
		t.Fatalf("recorded %#v", clicks)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.posts) != 0 {
		t.Fatalf("click answered through the API beyond the ack: %d posts", len(s.posts))
	}
	if len(s.callbacks) != 1 || s.callbacks[0]["type"] != float64(4) {
		t.Fatalf("callbacks = %#v", s.callbacks)
	}
}

// TestPruneDeletesOnlyOldOwnMessages pins the gc pass: old bot messages
// go, the starter (id == thread id), other authors, and fresh messages
// stay.
func TestPruneDeletesOnlyOldOwnMessages(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	fresh := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	mk := func(id, author string, ts string) Message {
		tm, _ := time.Parse(time.RFC3339, ts)
		return Message{ID: id, ChannelID: "4242", Author: Author{ID: author}, Timestamp: tm}
	}
	s := &testThreadServer{messages: []Message{
		mk("600", "bot-1", fresh),
		mk("500", "bot-1", old),
		mk("499", "someone-else", old),
		mk("4242", "bot-1", "2020-01-01T00:00:00Z"),
	}}
	srv := s.server(t)
	defer srv.Close()
	pruned, err := testThreadClient(srv.URL).PruneTestMessages(context.Background(), "4242", time.Now().Add(-24*time.Hour), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 1 || pruned[0] != "500" {
		t.Fatalf("pruned = %v", pruned)
	}
	if len(s.deletes) != 1 || !strings.HasSuffix(s.deletes[0], "/messages/500") {
		t.Fatalf("deletes = %v", s.deletes)
	}
	// Dry-run names the same message but deletes nothing.
	s2 := &testThreadServer{messages: s.messages}
	srv2 := s2.server(t)
	defer srv2.Close()
	pruned, err = testThreadClient(srv2.URL).PruneTestMessages(context.Background(), "4242", time.Now().Add(-24*time.Hour), true)
	if err != nil || len(pruned) != 1 || pruned[0] != "500" || len(s2.deletes) != 0 {
		t.Fatalf("dry-run pruned = %v deletes = %v err = %v", pruned, s2.deletes, err)
	}
}
