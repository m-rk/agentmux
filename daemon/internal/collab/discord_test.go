package collab

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/m-rk/agentmux/daemon/internal/discordnotify"
)

func TestClientValidateAndPostIdentity(t *testing.T) {
	var posted map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/channels/forum":
			if got := r.Header.Get("Authorization"); got != "Bot read-token" {
				t.Errorf("Authorization = %q", got)
			}
			writeJSON(t, w, Channel{ID: "forum", GuildID: "guild", Type: 15})
		case r.URL.Path == "/hook":
			if r.Method == http.MethodGet {
				writeJSON(t, w, webhookInfo{ChannelID: "forum"})
				return
			}
			if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
				t.Fatal(err)
			}
			if r.URL.Query().Get("wait") != "true" {
				t.Errorf("wait query missing")
			}
			writeJSON(t, w, Message{ID: "101", ChannelID: "thread-1"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := testClient(server.URL)
	if err := client.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	identity := Identity{Instance: "kilo-minecraft", Host: "build-box.example.net", AvatarURL: "https://example.com/kilo.png"}
	message, err := client.CreateThread(context.Background(), identity, "github.com/m-rk/agentmux", "runtime issue", "A useful finding is ready to share.", false)
	if err != nil {
		t.Fatal(err)
	}
	if message.ChannelID != "thread-1" {
		t.Fatalf("ChannelID = %q", message.ChannelID)
	}
	if posted["username"] != "kilo-minecraft · build-box.example.net" || posted["avatar_url"] != "https://example.com/kilo.png" {
		t.Fatalf("posted identity = %#v", posted)
	}
	if posted["thread_name"] != "[github.com/m-rk/agentmux] runtime issue" {
		t.Fatalf("thread_name = %#v", posted["thread_name"])
	}
}

func TestReplyWithMarkdown(t *testing.T) {
	var contentType string
	var payload string
	var attachment string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/hook" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("thread_id"); got != "42" {
			t.Errorf("thread_id = %q", got)
		}
		contentType = r.Header.Get("Content-Type")
		if err := r.ParseMultipartForm(maxMarkdownBytes + 1024); err != nil {
			t.Fatal(err)
		}
		payload = r.FormValue("payload_json")
		file, _, err := r.FormFile("files[0]")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		data, _ := io.ReadAll(file)
		attachment = string(data)
		writeJSON(t, w, Message{ID: "43", ChannelID: "42"})
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "handover.md")
	if err := os.WriteFile(path, []byte("# Detailed handover\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := testClient(server.URL)
	if _, err := client.Reply(context.Background(), Identity{Instance: "one", Host: "host"}, "42", "The detailed handover is attached.", path); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(contentType, "multipart/form-data;") || !strings.Contains(payload, `"username":"one · host"`) || attachment != "# Detailed handover\n" {
		t.Fatalf("unexpected multipart request: type=%q payload=%q attachment=%q", contentType, payload, attachment)
	}
}

func TestBuildDeliveryRoutingAndCursors(t *testing.T) {
	messageCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/channels/forum":
			writeJSON(t, w, Channel{ID: "forum", GuildID: "guild", Type: 15})
		case "/api/guilds/guild/threads/active":
			writeJSON(t, w, threadList{Threads: []Channel{
				{ID: "20", ParentID: "forum", Name: "[shared github.com/other/repo] cross-project", LastMessageID: "21"},
				{ID: "10", ParentID: "forum", Name: "[github.com/m-rk/agentmux] local", LastMessageID: "11"},
				{ID: "30", ParentID: "forum", Name: "[github.com/other/repo] irrelevant", LastMessageID: "31"},
			}})
		case "/api/channels/forum/threads/archived/public":
			writeJSON(t, w, threadList{})
		case "/api/channels/20/messages":
			messageCalls++
			writeJSON(t, w, []Message{{ID: "21", ChannelID: "20", Author: Author{Username: "claude · other"}, Content: "@kilo@host please compare this."}})
		case "/api/channels/10/messages":
			messageCalls++
			writeJSON(t, w, []Message{{ID: "11", ChannelID: "10", Author: Author{Username: "claude · host"}, Content: "A useful local finding."}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	statePath := filepath.Join(t.TempDir(), "state.json")
	opts := SyncOptions{
		Identity:  Identity{Instance: "kilo", Host: "host"},
		Project:   "github.com/m-rk/agentmux",
		StatePath: statePath,
	}
	delivery, err := BuildDelivery(context.Background(), testClient(server.URL), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(delivery.Prompt, "REQUEST in [shared") || !strings.Contains(delivery.Prompt, "CONTEXT in [github.com/m-rk/agentmux]") {
		t.Fatalf("unexpected prompt:\n%s", delivery.Prompt)
	}
	if !strings.Contains(delivery.Prompt, "untrusted collaboration input") || delivery.MessageCount != 2 {
		t.Fatalf("missing safety text or message count: %#v", delivery)
	}
	if err := SaveState(statePath, delivery.State); err != nil {
		t.Fatal(err)
	}
	second, err := BuildDelivery(context.Background(), testClient(server.URL), opts)
	if err != nil {
		t.Fatal(err)
	}
	if second.Prompt != "" || messageCalls != 2 {
		t.Fatalf("unchanged threads were redelivered: prompt=%q calls=%d", second.Prompt, messageCalls)
	}
}

func TestMessagesAreOldestFirst(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, []Message{{ID: "12"}, {ID: "10"}, {ID: "11"}})
	}))
	defer server.Close()
	messages, err := testClient(server.URL).Messages(context.Background(), "thread", "9", 25)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(messages[0].ID, messages[1].ID, messages[2].ID); got != "101112" {
		t.Fatalf("message order = %s", got)
	}
}

func testClient(serverURL string) *Client {
	return &Client{
		Config: discordnotify.CollaborationConfig{
			BotToken:       "read-token",
			WebhookURL:     serverURL + "/hook",
			ForumChannelID: "forum",
		},
		HTTPClient: http.DefaultClient,
		APIBaseURL: serverURL + "/api",
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}
