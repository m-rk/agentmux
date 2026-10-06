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

// buttonStyleServer serves a fake guild emoji list alongside the canned ask
// forum, so button posts resolve custom emoji by name without real ids.
type buttonStyleServer struct {
	mu         sync.Mutex
	emojiCalls int
	emojis     []GuildEmoji
	botPosts   []map[string]any
}

func (b *buttonStyleServer) server(t *testing.T) *httptest.Server {
	forum := Channel{ID: "forum", GuildID: "guild", Type: 15, AvailableTags: []ForumTag{
		{"t-task", "task"}, {"t-epic", "epic"}, {"t-idea", "idea"},
		{"t-needsme", "needs me"}, {"t-working", "working"}, {"t-blocked", "blocked"},
		{"t-parked", "parked"}, {"t-notnow", "not now"}, {"t-done", "done"}, {"t-failed", "failed"},
		{"t-ask", "ask"}, {"t-pending", "pending"}, {"t-answered", "answered"}, {"t-launched", "launched"},
		{"t-proj", "mergentic"},
	}}
	if b.emojis == nil {
		b.emojis = []GuildEmoji{{ID: "emoji-amp-id", Name: "amp"}, {ID: "emoji-claude-id", Name: "claude"}}
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		path := r.URL.EscapedPath()
		switch {
		case path == "/api/guilds/guild/emojis" && r.Method == http.MethodGet:
			b.emojiCalls++
			writeJSON(t, w, b.emojis)
		case path == "/api/channels/forum/threads" && r.Method == http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			b.botPosts = append(b.botPosts, body)
			writeJSON(t, w, Channel{ID: "900"})
		case path == "/api/channels/forum":
			writeJSON(t, w, forum)
		default:
			http.NotFound(w, r)
		}
	}))
}

func buttonRow(t *testing.T, s *buttonStyleServer) []any {
	t.Helper()
	m := s.botPosts[0]["message"].(map[string]any)
	return m["components"].([]any)[0].(map[string]any)["components"].([]any)
}

func TestPostAskButtonEmojiAndStylePayload(t *testing.T) {
	b := &buttonStyleServer{}
	s := b.server(t)
	defer s.Close()
	thread, msg, err := asksClientFor(s.URL).PostAsk(context.Background(), "T", "pick", nil, AskOptions{Buttons: []AskButton{
		{Label: "amp medium", Emoji: ":amp:", Style: "primary"},
		{Label: "pause", Emoji: "⏸️", Style: "danger"},
		{Label: "plain"},
	}})
	if err != nil || thread != "900" || msg != "900" {
		t.Fatalf("got %q %q %v", thread, msg, err)
	}
	row := buttonRow(t, b)
	first, second, third := row[0].(map[string]any), row[1].(map[string]any), row[2].(map[string]any)
	if first["custom_id"] != "ask:amp medium" || first["style"] != float64(1) {
		t.Fatalf("first = %#v", first)
	}
	emoji := first["emoji"].(map[string]any)
	if emoji["id"] != "emoji-amp-id" || emoji["name"] != "amp" {
		t.Fatalf("emoji = %#v", emoji)
	}
	if second["style"] != float64(4) || second["emoji"].(map[string]any)["name"] != "⏸️" {
		t.Fatalf("second = %#v", second)
	}
	// Default style is secondary (grey); no emoji key when none given.
	if third["style"] != float64(2) {
		t.Fatalf("third = %#v", third)
	}
	if _, ok := third["emoji"]; ok {
		t.Fatalf("plain button carries emoji: %#v", third)
	}
}

func TestPostAskCustomEmojiCachedAcrossPosts(t *testing.T) {
	b := &buttonStyleServer{}
	s := b.server(t)
	defer s.Close()
	c := asksClientFor(s.URL)
	opts := AskOptions{Buttons: []AskButton{{Label: "a", Emoji: ":amp:"}, {Label: "b", Emoji: ":claude:"}}}
	if _, _, err := c.PostAsk(context.Background(), "T1", "pick", nil, opts); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.PostAsk(context.Background(), "T2", "pick", nil, opts); err != nil {
		t.Fatal(err)
	}
	if b.emojiCalls != 1 {
		t.Fatalf("guild emoji listed %d times, want 1", b.emojiCalls)
	}
}

func TestPostAskUnknownCustomEmojiPostsWithoutEmoji(t *testing.T) {
	b := &buttonStyleServer{}
	s := b.server(t)
	defer s.Close()
	if _, _, err := asksClientFor(s.URL).PostAsk(context.Background(), "T", "pick", nil,
		AskOptions{Buttons: []AskButton{{Label: "a", Emoji: ":nope:"}}}); err != nil {
		t.Fatal(err)
	}
	btn := buttonRow(t, b)[0].(map[string]any)
	if _, ok := btn["emoji"]; ok {
		t.Fatalf("unknown emoji kept: %#v", btn)
	}
}

func TestPostAskDuplicateLabelsRefused(t *testing.T) {
	b := &buttonStyleServer{}
	s := b.server(t)
	defer s.Close()
	_, _, err := asksClientFor(s.URL).PostAsk(context.Background(), "T", "pick", nil,
		AskOptions{Buttons: []AskButton{{Label: "same"}, {Label: "same"}}})
	if err == nil || !strings.Contains(err.Error(), `duplicate button "same"`) || len(b.botPosts) != 0 {
		t.Fatalf("err = %v posts = %d", err, len(b.botPosts))
	}
}

func TestButtonStyleNames(t *testing.T) {
	for name, want := range map[string]int{"": 2, "secondary": 2, "primary": 1, "success": 3, "danger": 4, "Primary": 1} {
		got, err := buttonStyleNumber(name)
		if err != nil || got != want {
			t.Errorf("style %q = %d, %v; want %d", name, got, err, want)
		}
	}
	if _, err := buttonStyleNumber("rainbow"); err == nil {
		t.Error("accepted unknown style")
	}
}
