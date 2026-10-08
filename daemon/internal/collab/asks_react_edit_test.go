package collab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// reactEditServer records bot reaction PUTs and message PATCHes, and serves a
// canned ask thread plus one message with buttons.
type reactEditServer struct {
	mu      sync.Mutex
	puts    []string // reaction PUT paths, in order
	deletes []string // reaction DELETE paths, in order
	patches []string // message PATCH paths, in order
	bodies  []map[string]any

	message   map[string]any
	reactions []MessageReaction
	threads   map[string]Channel
}

func (s *reactEditServer) server(t *testing.T) *httptest.Server {
	forum := Channel{ID: "forum", GuildID: "guild", Type: 15, AvailableTags: []ForumTag{
		{"t-task", "task"}, {"t-epic", "epic"}, {"t-idea", "idea"},
		{"t-needsme", "needs me"}, {"t-working", "working"}, {"t-blocked", "blocked"},
		{"t-parked", "parked"}, {"t-notnow", "not now"}, {"t-done", "done"}, {"t-failed", "failed"},
		{"t-ask", "ask"}, {"t-pending", "pending"}, {"t-answered", "answered"}, {"t-launched", "launched"},
		{"t-proj", "mergentic"},
	}}
	if s.threads == nil {
		s.threads = map[string]Channel{"900": {ID: "900", ParentID: "forum", AppliedTags: []string{"t-ask"}}}
	}
	if s.message == nil {
		s.message = map[string]any{"id": "500", "content": "pick one", "reactions": s.reactions, "components": []any{
			map[string]any{"type": 1, "components": []any{
				map[string]any{"type": 2, "style": 2, "label": "Ship it", "custom_id": "ask:Ship it"},
				map[string]any{"type": 2, "style": 2, "label": "Not now", "custom_id": "ask:Not now"},
			}},
		}}
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		path := r.URL.EscapedPath()
		switch {
		case r.Method == http.MethodPut && strings.Contains(path, "/reactions/"):
			s.puts = append(s.puts, path)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && strings.Contains(path, "/reactions/"):
			s.deletes = append(s.deletes, path)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPatch && strings.Contains(path, "/messages/"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.patches = append(s.patches, path)
			s.bodies = append(s.bodies, body)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodGet && strings.Contains(path, "/messages/"):
			writeJSON(t, w, s.message)
		case path == "/api/channels/forum":
			writeJSON(t, w, forum)
		case strings.HasPrefix(path, "/api/channels/"):
			id := strings.TrimPrefix(path, "/api/channels/")
			if th, ok := s.threads[id]; ok {
				writeJSON(t, w, th)
				return
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestReactAskPutsReaction(t *testing.T) {
	s := &reactEditServer{}
	srv := s.server(t)
	defer srv.Close()
	if err := asksClientFor(srv.URL).ReactAsk(context.Background(), "900", "500", "🤖"); err != nil {
		t.Fatal(err)
	}
	want := "/api/channels/900/messages/500/reactions/" + url.PathEscape("🤖") + "/@me"
	if len(s.puts) != 1 || s.puts[0] != want {
		t.Fatalf("puts = %v, want %s", s.puts, want)
	}
}

func TestReactAskReplaceRemovesOnlyOwnReactionsBeforeAdding(t *testing.T) {
	s := &reactEditServer{reactions: []MessageReaction{
		{Me: true, Emoji: struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}{Name: "1️⃣"}},
		{Me: true, Emoji: struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}{ID: "1234", Name: "party"}},
		{Me: false, Emoji: struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}{Name: "👀"}},
	}}
	srv := s.server(t)
	defer srv.Close()
	if err := asksClientFor(srv.URL).ReactAskWithReplace(context.Background(), "900", "500", "✅", true); err != nil {
		t.Fatal(err)
	}
	wantDeletes := []string{
		"/api/channels/900/messages/500/reactions/" + url.PathEscape("1️⃣") + "/@me",
		"/api/channels/900/messages/500/reactions/" + url.PathEscape("party:1234") + "/@me",
	}
	if len(s.deletes) != len(wantDeletes) || s.deletes[0] != wantDeletes[0] || s.deletes[1] != wantDeletes[1] {
		t.Fatalf("deletes = %v, want %v", s.deletes, wantDeletes)
	}
	wantPut := "/api/channels/900/messages/500/reactions/" + url.PathEscape("✅") + "/@me"
	if len(s.puts) != 1 || s.puts[0] != wantPut {
		t.Fatalf("puts = %v, want %s", s.puts, wantPut)
	}
}

func TestReactAskRefusesNonAskThread(t *testing.T) {
	s := &reactEditServer{threads: map[string]Channel{"901": {ID: "901", ParentID: "forum"}}}
	srv := s.server(t)
	defer srv.Close()
	if err := asksClientFor(srv.URL).ReactAsk(context.Background(), "901", "500", "🤖"); err == nil || len(s.puts) != 0 {
		t.Fatalf("err = %v puts = %v", err, s.puts)
	}
}

func TestReactAskRejectsBadEmoji(t *testing.T) {
	s := &reactEditServer{}
	srv := s.server(t)
	defer srv.Close()
	for _, emoji := range []string{"", "a b", "a/b"} {
		if err := asksClientFor(srv.URL).ReactAsk(context.Background(), "900", "500", emoji); err == nil || len(s.puts) != 0 {
			t.Fatalf("emoji %q: err = %v puts = %v", emoji, err, s.puts)
		}
	}
}

func TestEditAskBodyOnly(t *testing.T) {
	s := &reactEditServer{}
	srv := s.server(t)
	defer srv.Close()
	if err := asksClientFor(srv.URL).EditAsk(context.Background(), "900", "500", EditAskOptions{Body: "updated"}); err != nil {
		t.Fatal(err)
	}
	if len(s.patches) != 1 || s.patches[0] != "/api/channels/900/messages/500" {
		t.Fatalf("patches = %v", s.patches)
	}
	if s.bodies[0]["content"] != "updated" {
		t.Fatalf("body = %#v", s.bodies[0])
	}
	if _, ok := s.bodies[0]["components"]; ok {
		t.Fatalf("body-only edit restyled components: %#v", s.bodies[0])
	}
}

func TestEditAskDisableButtons(t *testing.T) {
	s := &reactEditServer{}
	srv := s.server(t)
	defer srv.Close()
	if err := asksClientFor(srv.URL).EditAsk(context.Background(), "900", "500",
		EditAskOptions{Body: "done", DisableButtons: true}); err != nil {
		t.Fatal(err)
	}
	rows := s.bodies[0]["components"].([]any)
	row := rows[0].(map[string]any)["components"].([]any)
	for _, b := range row {
		btn := b.(map[string]any)
		if btn["disabled"] != true || btn["style"] != float64(2) {
			t.Fatalf("button = %#v", btn)
		}
	}
	if s.bodies[0]["content"] != "done" {
		t.Fatalf("body = %#v", s.bodies[0])
	}
}

func TestEditAskChosenHighlightsOne(t *testing.T) {
	s := &reactEditServer{}
	srv := s.server(t)
	defer srv.Close()
	if err := asksClientFor(srv.URL).EditAsk(context.Background(), "900", "500",
		EditAskOptions{Chosen: "Ship it"}); err != nil {
		t.Fatal(err)
	}
	rows := s.bodies[0]["components"].([]any)
	row := rows[0].(map[string]any)["components"].([]any)
	chosen, other := row[0].(map[string]any), row[1].(map[string]any)
	if chosen["disabled"] != true || chosen["style"] != float64(3) || chosen["label"] != "Ship it ✓" {
		t.Fatalf("chosen = %#v", chosen)
	}
	if other["disabled"] != true || other["style"] != float64(2) || other["label"] != "Not now" {
		t.Fatalf("other = %#v", other)
	}
	if _, ok := s.bodies[0]["content"]; ok {
		t.Fatalf("chosen-only edit rewrote content: %#v", s.bodies[0])
	}
}

func TestEditAskKeepsButtonEmoji(t *testing.T) {
	s := &reactEditServer{message: map[string]any{"id": "500", "content": "pick one", "components": []any{
		map[string]any{"type": 1, "components": []any{
			map[string]any{"type": 2, "style": 1, "label": "amp medium", "custom_id": "ask:amp medium",
				"emoji": map[string]any{"id": "emoji-amp-id", "name": "amp"}},
			map[string]any{"type": 2, "style": 2, "label": "Not now", "custom_id": "ask:Not now",
				"emoji": map[string]any{"name": "⏸️"}},
		}},
	}}}
	srv := s.server(t)
	defer srv.Close()
	if err := asksClientFor(srv.URL).EditAsk(context.Background(), "900", "500",
		EditAskOptions{Chosen: "amp medium"}); err != nil {
		t.Fatal(err)
	}
	row := s.bodies[0]["components"].([]any)[0].(map[string]any)["components"].([]any)
	chosen, other := row[0].(map[string]any), row[1].(map[string]any)
	if chosen["emoji"].(map[string]any)["id"] != "emoji-amp-id" {
		t.Fatalf("chosen lost emoji: %#v", chosen)
	}
	if other["emoji"].(map[string]any)["name"] != "⏸️" || other["style"] != float64(2) {
		t.Fatalf("other lost emoji or style: %#v", other)
	}
}

func TestEditAskChosenMustExist(t *testing.T) {
	s := &reactEditServer{}
	srv := s.server(t)
	defer srv.Close()
	if err := asksClientFor(srv.URL).EditAsk(context.Background(), "900", "500",
		EditAskOptions{Chosen: "Bogus"}); err == nil || len(s.patches) != 0 {
		t.Fatalf("err = %v patches = %v", err, s.patches)
	}
}

func TestEditAskNoButtonsFails(t *testing.T) {
	s := &reactEditServer{message: map[string]any{"id": "500", "content": "plain"}}
	srv := s.server(t)
	defer srv.Close()
	if err := asksClientFor(srv.URL).EditAsk(context.Background(), "900", "500",
		EditAskOptions{DisableButtons: true}); err == nil || len(s.patches) != 0 {
		t.Fatalf("err = %v patches = %v", err, s.patches)
	}
}

func TestEditAskRefusesNonAskThreadAndEmpty(t *testing.T) {
	s := &reactEditServer{threads: map[string]Channel{
		"900": {ID: "900", ParentID: "forum", AppliedTags: []string{"t-ask"}},
		"901": {ID: "901", ParentID: "forum"},
	}}
	srv := s.server(t)
	defer srv.Close()
	c := asksClientFor(srv.URL)
	if err := c.EditAsk(context.Background(), "901", "500", EditAskOptions{Body: "x"}); err == nil || len(s.patches) != 0 {
		t.Fatalf("non-ask edit allowed: %v", err)
	}
	if err := c.EditAsk(context.Background(), "900", "500", EditAskOptions{}); err == nil {
		t.Fatal("empty edit allowed")
	}
	// Only the body is rewritten: no mention is re-added on edit.
	if err := c.EditAsk(context.Background(), "900", "500", EditAskOptions{Body: "<@777> ping"}); err != nil {
		t.Fatal(err)
	}
	if s.bodies[0]["content"] != "<@777> ping" {
		t.Fatalf("body = %#v", s.bodies[0])
	}
	if _, ok := s.bodies[0]["allowed_mentions"]; ok {
		t.Fatalf("edit set allowed_mentions: %#v", s.bodies[0])
	}
}
