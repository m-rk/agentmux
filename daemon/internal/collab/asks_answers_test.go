package collab

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// answersServer records bot calls and serves canned reactions.
type answersServer struct {
	mu        sync.Mutex
	puts      []string // reaction PUT paths, in order
	botPosts  []map[string]any
	botPaths  []string
	putStatus int
	messages  []Message
	reactors  map[string][]Author // keyed by decoded emoji
	callbacks []map[string]any
	cbPaths   []string
}

func (a *answersServer) server(t *testing.T) *httptest.Server {
	forum := Channel{ID: "forum", GuildID: "guild", Type: 15, AvailableTags: []ForumTag{
		{"t-task", "task"}, {"t-epic", "epic"}, {"t-idea", "idea"},
		{"t-needsme", "needs me"}, {"t-working", "working"}, {"t-blocked", "blocked"},
		{"t-parked", "parked"}, {"t-notnow", "not now"}, {"t-done", "done"}, {"t-failed", "failed"},
		{"t-ask", "ask"}, {"t-pending", "pending"}, {"t-answered", "answered"}, {"t-launched", "launched"},
		{"t-proj", "mergentic"},
	}}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		path := r.URL.EscapedPath()
		switch {
		case r.Method == http.MethodPut && strings.Contains(path, "/reactions/"):
			a.puts = append(a.puts, path)
			if a.putStatus != 0 {
				w.WriteHeader(a.putStatus)
				_, _ = w.Write([]byte(`{"message":"Missing Permissions"}`))
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && strings.Contains(path, "/reactions/"):
			emoji, _ := url.PathUnescape(path[strings.Index(path, "/reactions/")+len("/reactions/"):])
			writeJSON(t, w, a.reactors[emoji])
		case path == "/api/channels/forum/threads" && r.Method == http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			a.botPosts, a.botPaths = append(a.botPosts, body), append(a.botPaths, path)
			writeJSON(t, w, Channel{ID: "900"})
		case path == "/api/channels/900/messages" && r.Method == http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			a.botPosts, a.botPaths = append(a.botPosts, body), append(a.botPaths, path)
			writeJSON(t, w, Message{ID: "501", ChannelID: "900"})
		case path == "/api/channels/900/messages":
			writeJSON(t, w, a.messages)
		case path == "/api/channels/forum":
			writeJSON(t, w, forum)
		case path == "/api/channels/900":
			writeJSON(t, w, Channel{ID: "900", ParentID: "forum", AppliedTags: []string{"t-ask"}})
		case path == "/hook" && r.Method == http.MethodPost:
			writeJSON(t, w, Message{ID: "500", ChannelID: "900"})
		case strings.HasPrefix(path, "/api/interactions/"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			a.callbacks, a.cbPaths = append(a.callbacks, body), append(a.cbPaths, path)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestPostAskSeedsReactionsInOrder(t *testing.T) {
	a := &answersServer{}
	s := a.server(t)
	defer s.Close()
	thread, msg, err := asksClientFor(s.URL).PostAsk(context.Background(), "T", "b", nil, AskOptions{Reactions: []string{"1️⃣", "2️⃣", "⏸️"}})
	if err != nil || thread != "900" || msg != "500" {
		t.Fatalf("got %q %q %v", thread, msg, err)
	}
	if len(a.puts) != 3 {
		t.Fatalf("puts = %v", a.puts)
	}
	for i, emoji := range []string{"1️⃣", "2️⃣", "⏸️"} {
		want := "/api/channels/900/messages/500/reactions/" + url.PathEscape(emoji) + "/@me"
		if a.puts[i] != want {
			t.Fatalf("put %d = %s, want %s", i, a.puts[i], want)
		}
	}
}

func TestPostAskReactionFailureKeepsIDs(t *testing.T) {
	a := &answersServer{putStatus: http.StatusForbidden}
	s := a.server(t)
	defer s.Close()
	thread, msg, err := asksClientFor(s.URL).PostAsk(context.Background(), "T", "b", nil, AskOptions{Reactions: []string{"1️⃣"}})
	var seed *ReactionSeedError
	if !errors.As(err, &seed) || thread != "900" || msg != "500" {
		t.Fatalf("got %q %q %v", thread, msg, err)
	}
}

func TestAskOptionsValidation(t *testing.T) {
	for _, o := range []AskOptions{
		{Reactions: []string{"a b"}},
		{Reactions: []string{"1️⃣", "1️⃣"}},
		{Buttons: []AskButton{{Label: ""}}},
		{Buttons: []AskButton{{Label: "x"}, {Label: "x"}}},
		{Buttons: make([]AskButton, 26)},
		{Buttons: []AskButton{{Label: "x", Style: "rainbow"}}},
	} {
		if o.validate() == nil {
			t.Errorf("accepted %#v", o)
		}
	}
}

func TestPostAskWithButtonsUsesBot(t *testing.T) {
	a := &answersServer{}
	s := a.server(t)
	defer s.Close()
	thread, msg, err := asksClientFor(s.URL).PostAsk(context.Background(), "T", "pick", nil, AskOptions{Buttons: []AskButton{{Label: "Ship it"}, {Label: "Not now"}}})
	if err != nil || thread != "900" || msg != "900" {
		t.Fatalf("got %q %q %v", thread, msg, err)
	}
	m := a.botPosts[0]["message"].(map[string]any)
	if !strings.HasPrefix(m["content"].(string), "<@777>\n") {
		t.Fatalf("content = %v", m["content"])
	}
	row := m["components"].([]any)[0].(map[string]any)["components"].([]any)
	if len(row) != 2 || row[0].(map[string]any)["custom_id"] != "ask:Ship it" || row[1].(map[string]any)["label"] != "Not now" {
		t.Fatalf("row = %#v", row)
	}
}

func TestPostAskInThreadWithButtons(t *testing.T) {
	a := &answersServer{}
	s := a.server(t)
	defer s.Close()
	id, err := asksClientFor(s.URL).PostAskInThread(context.Background(), "900", "", "q", nil, AskOptions{Buttons: []AskButton{{Label: "A"}}})
	if err != nil || id != "501" || a.botPaths[0] != "/api/channels/900/messages" {
		t.Fatalf("id = %q err = %v paths = %v", id, err, a.botPaths)
	}
}

func TestReadAskReportsOnlyConfiguredUsersReactions(t *testing.T) {
	var one, two MessageReaction
	one.Count, one.Me, one.Emoji.Name = 2, true, "1️⃣"
	two.Count, two.Me, two.Emoji.Name = 1, true, "2️⃣" // bot's own seed only
	a := &answersServer{
		messages: []Message{{ID: "500", WebhookID: "wh", Reactions: []MessageReaction{one, two}}},
		reactors: map[string][]Author{"1️⃣": {{ID: "bot"}, {ID: "777"}}, "2️⃣": {{ID: "bot"}}},
	}
	s := a.server(t)
	defer s.Close()
	got, err := asksClientFor(s.URL).ReadAsk(context.Background(), "900", "")
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v %v", got, err)
	}
	if len(got[0].Answers) != 1 || got[0].Answers[0] != (AskAnswer{Kind: "reaction", Value: "1️⃣", MessageID: "500"}) {
		t.Fatalf("answers = %#v", got[0].Answers)
	}
}

func TestReadAskIgnoresOtherUsersReactions(t *testing.T) {
	var r MessageReaction
	r.Count, r.Emoji.Name = 1, "1️⃣"
	a := &answersServer{
		messages: []Message{{ID: "500", Reactions: []MessageReaction{r}}},
		reactors: map[string][]Author{"1️⃣": {{ID: "778"}}},
	}
	s := a.server(t)
	defer s.Close()
	got, err := asksClientFor(s.URL).ReadAsk(context.Background(), "900", "")
	if err != nil || len(got[0].Answers) != 0 {
		t.Fatalf("got %#v %v", got, err)
	}
}

func TestReadAskIncludesClicksFromConfiguredUser(t *testing.T) {
	a := &answersServer{messages: []Message{{ID: "500"}, {ID: "501"}}}
	s := a.server(t)
	defer s.Close()
	c := asksClientFor(s.URL)
	c.ClicksPath = filepath.Join(t.TempDir(), "clicks.jsonl")
	store := &ClickStore{Path: c.ClicksPath}
	now := time.Now().UTC()
	_, _ = store.Append(Click{ThreadID: "900", MessageID: "500", Label: "Ship it", UserID: "777", Timestamp: now})
	_, _ = store.Append(Click{ThreadID: "900", MessageID: "501", Label: "Spoof", UserID: "778", Timestamp: now})
	_, _ = store.Append(Click{ThreadID: "901", MessageID: "502", Label: "Elsewhere", UserID: "777", Timestamp: now})
	got, err := c.ReadAsk(context.Background(), "900", "")
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v %v", got, err)
	}
	if len(got[0].Answers) != 1 || got[0].Answers[0].Kind != "button" || got[0].Answers[0].Value != "Ship it" || got[0].Answers[0].MessageID != "500" || len(got[1].Answers) != 0 {
		t.Fatalf("answers = %#v / %#v", got[0].Answers, got[1].Answers)
	}
}

func TestReadAskAfterIncludesAnsweredAnchor(t *testing.T) {
	// Answers to the ask live on the message the caller reads *after*.
	var r MessageReaction
	r.Count, r.Emoji.Name = 1, "1️⃣"
	anchor := Message{ID: "500", Reactions: []MessageReaction{r}}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/api/channels/forum":
			writeJSON(t, w, Channel{ID: "forum", AvailableTags: []ForumTag{
				{"t-task", "task"}, {"t-ask", "ask"},
			}})
		case "/api/channels/900":
			writeJSON(t, w, Channel{ID: "900", ParentID: "forum", AppliedTags: []string{"t-ask"}})
		case "/api/channels/900/messages/500":
			writeJSON(t, w, anchor)
		case "/api/channels/900/messages":
			writeJSON(t, w, []Message{})
		default:
			if strings.Contains(req.URL.Path, "/reactions/") {
				writeJSON(t, w, []Author{{ID: "777"}})
				return
			}
			http.NotFound(w, req)
		}
	}))
	defer s.Close()
	got, err := asksClientFor(s.URL).ReadAsk(context.Background(), "900", "500")
	if err != nil || len(got) != 1 || len(got[0].Answers) != 1 || got[0].Answers[0].Value != "1️⃣" {
		t.Fatalf("got %#v %v", got, err)
	}
}

func TestClickStoreFirstClickWins(t *testing.T) {
	store := &ClickStore{Path: filepath.Join(t.TempDir(), "sub", "clicks.jsonl")}
	ok, err := store.Append(Click{ThreadID: "900", MessageID: "500", Label: "A", UserID: "777"})
	if !ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, _ := store.Append(Click{ThreadID: "900", MessageID: "500", Label: "B", UserID: "777"}); ok {
		t.Fatal("second click recorded")
	}
}

func interactionJSON(t *testing.T, user, parent, customID string) json.RawMessage {
	raw, err := json.Marshal(map[string]any{
		"id": "i1", "token": "tok", "type": 3, "channel_id": "900",
		"channel": map[string]any{"parent_id": parent},
		"member":  map[string]any{"user": map[string]any{"id": user}},
		"data":    map[string]any{"custom_id": customID},
		"message": map[string]any{"id": "500", "components": []any{map[string]any{"type": 1, "components": []any{
			map[string]any{"type": 2, "style": 1, "label": "Ship it", "custom_id": "ask:Ship it",
				"emoji": map[string]any{"id": "emoji-amp-id", "name": "amp"}},
			map[string]any{"type": 2, "style": 2, "label": "Not now", "custom_id": "ask:Not now"},
		}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func newListener(t *testing.T, s *httptest.Server) *Listener {
	return &Listener{Client: asksClientFor(s.URL), Store: &ClickStore{Path: filepath.Join(t.TempDir(), "clicks.jsonl")}}
}

func TestHandleInteractionRecordsAndDisablesButtons(t *testing.T) {
	a := &answersServer{}
	s := a.server(t)
	defer s.Close()
	l := newListener(t, s)
	if err := l.HandleInteraction(context.Background(), interactionJSON(t, "777", "forum", "ask:Ship it")); err != nil {
		t.Fatal(err)
	}
	clicks, _ := l.Store.load()
	if len(clicks) != 1 || clicks[0].Label != "Ship it" || clicks[0].MessageID != "500" || clicks[0].ThreadID != "900" {
		t.Fatalf("clicks = %#v", clicks)
	}
	if a.cbPaths[0] != "/api/interactions/i1/tok/callback" || a.callbacks[0]["type"] != float64(7) {
		t.Fatalf("callback = %v %#v", a.cbPaths, a.callbacks)
	}
	row := a.callbacks[0]["data"].(map[string]any)["components"].([]any)[0].(map[string]any)["components"].([]any)
	chosen, other := row[0].(map[string]any), row[1].(map[string]any)
	if chosen["disabled"] != true || other["disabled"] != true || chosen["style"] != float64(3) || chosen["label"] != "Ship it ✓" || other["style"] != float64(2) {
		t.Fatalf("row = %#v", row)
	}
	// The click ack keeps the button's emoji; clicks still map by custom id.
	if chosen["emoji"].(map[string]any)["id"] != "emoji-amp-id" {
		t.Fatalf("click ack lost emoji: %#v", chosen)
	}
}

func TestHandleInteractionIgnoresOtherUsers(t *testing.T) {
	a := &answersServer{}
	s := a.server(t)
	defer s.Close()
	l := newListener(t, s)
	if err := l.HandleInteraction(context.Background(), interactionJSON(t, "778", "forum", "ask:Ship it")); err != nil {
		t.Fatal(err)
	}
	if clicks, _ := l.Store.load(); len(clicks) != 0 {
		t.Fatalf("recorded %#v", clicks)
	}
	data := a.callbacks[0]["data"].(map[string]any)
	if a.callbacks[0]["type"] != float64(4) || data["flags"] != float64(64) {
		t.Fatalf("callback = %#v", a.callbacks[0])
	}
}

func TestHandleInteractionRefusesNonAskChannelAndSecondClick(t *testing.T) {
	a := &answersServer{}
	s := a.server(t)
	defer s.Close()
	l := newListener(t, s)
	if err := l.HandleInteraction(context.Background(), interactionJSON(t, "777", "elsewhere", "ask:Ship it")); err != nil {
		t.Fatal(err)
	}
	if clicks, _ := l.Store.load(); len(clicks) != 0 {
		t.Fatal("recorded click outside the forum")
	}
	_ = l.HandleInteraction(context.Background(), interactionJSON(t, "777", "forum", "ask:Ship it"))
	_ = l.HandleInteraction(context.Background(), interactionJSON(t, "777", "forum", "ask:Not now"))
	if clicks, _ := l.Store.load(); len(clicks) != 1 || clicks[0].Label != "Ship it" {
		t.Fatalf("clicks = %#v", clicks)
	}
	// Unrelated components are left alone.
	n := len(a.callbacks)
	_ = l.HandleInteraction(context.Background(), interactionJSON(t, "777", "forum", "other:thing"))
	if len(a.callbacks) != n {
		t.Fatal("responded to a foreign component")
	}
}

// TestListenerGatewayHandshake drives the real websocket loop against a fake
// gateway: HELLO, IDENTIFY, then an INTERACTION_CREATE that must be recorded.
func TestListenerGatewayHandshake(t *testing.T) {
	a := &answersServer{}
	api := a.server(t)
	defer api.Close()
	identified := make(chan map[string]any, 1)
	up := websocket.Upgrader{}
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 60000}})
		var f map[string]any
		if conn.ReadJSON(&f) != nil {
			return
		}
		identified <- f
		_ = conn.WriteJSON(map[string]any{"op": 0, "s": 1, "t": "READY", "d": map[string]any{"session_id": "s", "resume_gateway_url": "ws://unused"}})
		_ = conn.WriteJSON(map[string]any{"op": 0, "s": 2, "t": "INTERACTION_CREATE", "d": json.RawMessage(interactionJSON(t, "777", "forum", "ask:Ship it"))})
		time.Sleep(2 * time.Second)
	}))
	defer gw.Close()
	l := newListener(t, api)
	l.GatewayURL = "ws" + strings.TrimPrefix(gw.URL, "http")
	l.Logf = func(string, ...any) {}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Run(ctx)
	select {
	case f := <-identified:
		d := f["d"].(map[string]any)
		if f["op"] != float64(2) || d["intents"] != float64(0) || d["token"] != "read-token" {
			t.Fatalf("identify = %#v", f)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no IDENTIFY")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if clicks, _ := l.Store.load(); len(clicks) == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("click not recorded via gateway")
}
