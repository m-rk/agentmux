package collab

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type fakeAsks struct {
	mu          sync.Mutex
	hook        []map[string]any
	hookQ       []string
	patched     map[string]any
	rateLimited map[string]int
	fileName    string
	fileBytes   int
	requests    int
}

func (f *fakeAsks) server(t *testing.T, threads map[string]Channel, messages []Message) *httptest.Server {
	forum := Channel{ID: "forum", GuildID: "guild", Type: 15, AvailableTags: []ForumTag{
		{"t-task", "task"}, {"t-epic", "epic"}, {"t-idea", "idea"}, {"t-spike", "spike"}, {"t-bug", "bug"},
		{"t-needsme", "needs me"}, {"t-working", "working"}, {"t-blocked", "blocked"},
		{"t-parked", "parked"}, {"t-notnow", "not now"}, {"t-done", "done"}, {"t-failed", "failed"},
		{"t-ask", "ask"}, {"t-pending", "pending"}, {"t-answered", "answered"}, {"t-launched", "launched"},
		{"t-proj", "mergentic"},
	}}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests++
		switch {
		case r.URL.Path == "/api/channels/forum":
			writeJSON(t, w, forum)
		case r.URL.Path == "/hook" && r.Method == http.MethodPost:
			var body map[string]any
			if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
				_ = r.ParseMultipartForm(10 << 20)
				_ = json.Unmarshal([]byte(r.FormValue("payload_json")), &body)
				if file, header, err := r.FormFile("files[0]"); err == nil {
					f.fileName = header.Filename
					data, _ := io.ReadAll(file)
					f.fileBytes = len(data)
					file.Close()
				}
			} else {
				_ = json.NewDecoder(r.Body).Decode(&body)
			}
			f.hook = append(f.hook, body)
			f.hookQ = append(f.hookQ, r.URL.RawQuery)
			writeJSON(t, w, Message{ID: "500", ChannelID: "900", Attachments: []Attachment{{ID: "1", Filename: "image.png", URL: "https://cdn.discord.test/image.png"}}})
		case strings.HasSuffix(r.URL.Path, "/messages"):
			threadID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/channels/"), "/messages")
			if f.rateLimited != nil && f.rateLimited[threadID] > 0 {
				f.rateLimited[threadID]--
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"retry_after":0}`))
				return
			}
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

func TestReplyAskImageValidatesAndUploadsMultipart(t *testing.T) {
	f := &fakeAsks{}
	s := f.server(t, map[string]Channel{"900": {ID: "900", ParentID: "forum", AppliedTags: []string{"t-task"}}}, nil)
	defer s.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "private-name.png")
	data := encodeTestImage(t, "png")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	id, link, err := asksClientFor(s.URL).ReplyAskImage(context.Background(), "900", "diagram", path, false, false, "")
	if err != nil || id != "500" || link != "https://cdn.discord.test/image.png" {
		t.Fatalf("reply = %q %q %v", id, link, err)
	}
	if f.hook[0]["content"] != "diagram" || !strings.Contains(f.hookQ[0], "thread_id=900") || !strings.Contains(f.hookQ[0], "wait=true") {
		t.Fatalf("payload=%#v query=%q", f.hook[0], f.hookQ[0])
	}
	encoded, _ := json.Marshal(f.hook[0])
	if strings.Contains(string(encoded), dir) || strings.Contains(string(encoded), "private-name") {
		t.Fatal("local path or filename leaked")
	}
	if f.fileName != "image.png" || f.fileBytes != len(data) {
		t.Fatalf("uploaded file = %q (%d bytes), want generic image.png (%d bytes)", f.fileName, f.fileBytes, len(data))
	}
}

func TestReadAskImageSupportsPNGJPEGAndGIF(t *testing.T) {
	for _, format := range []string{"png", "jpeg", "gif"} {
		t.Run(format, func(t *testing.T) {
			data := encodeTestImage(t, format)
			path := filepath.Join(t.TempDir(), "input."+format)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			got, filename, err := readAskImage(path)
			if err != nil {
				t.Fatal(err)
			}
			wantName := map[string]string{"png": "image.png", "jpeg": "image.jpg", "gif": "image.gif"}[format]
			if filename != wantName || !bytes.Equal(got, data) {
				t.Fatalf("readAskImage = %q, %d bytes; want %q, %d bytes", filename, len(got), wantName, len(data))
			}
		})
	}
}

func encodeTestImage(t *testing.T, format string) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 32, G: 64, B: 96, A: 255})
	var out bytes.Buffer
	var err error
	switch format {
	case "png":
		err = png.Encode(&out, img)
	case "jpeg":
		err = jpeg.Encode(&out, img, &jpeg.Options{Quality: 90})
	case "gif":
		err = gif.Encode(&out, img, nil)
	default:
		t.Fatalf("unknown fixture format %q", format)
	}
	if err != nil {
		t.Fatalf("encoding %s fixture: %v", format, err)
	}
	return out.Bytes()
}

func TestReplyAskImageInvalidDoesNotContactDiscord(t *testing.T) {
	f := &fakeAsks{}
	s := f.server(t, nil, nil)
	defer s.Close()
	path := filepath.Join(t.TempDir(), "bad.png")
	if err := os.WriteFile(path, []byte("not an image"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := asksClientFor(s.URL).ReplyAskImage(context.Background(), "900", "caption", path, false, false, ""); err == nil {
		t.Fatal("invalid image accepted")
	}
	if f.requests != 0 {
		t.Fatal("Discord contacted for invalid image")
	}
}

func TestBatchReadAndHeadsPartialFailureAndRateLimit(t *testing.T) {
	f := &fakeAsks{rateLimited: map[string]int{"900": 1}}
	threads := map[string]Channel{
		"900": {ID: "900", ParentID: "forum", AppliedTags: []string{"t-task"}},
		"902": {ID: "902", ParentID: "forum", AppliedTags: []string{"t-task"}},
	}
	msgs := []Message{{ID: "41", Content: "latest", Author: Author{ID: "777", Username: "alice"}}}
	s := f.server(t, threads, msgs)
	defer s.Close()
	c := asksClientFor(s.URL)
	got := c.ReadAsks(context.Background(), []string{"900", "missing"}, map[string]string{"900": "40"})
	if got["900"].Error != "" || len(got["900"].Messages) != 1 || got["missing"].Error == "" {
		t.Fatalf("batch read = %#v", got)
	}
	heads := c.AskHeads(context.Background(), []string{"900", "902"})
	if heads["900"].LastMessageID != "41" || heads["902"].LastMessageID != "41" || heads["900"].Error != "" {
		t.Fatalf("heads = %#v", heads)
	}
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

func TestPostAskBugKind(t *testing.T) {
	f := &fakeAsks{}
	s := f.server(t, nil, nil)
	defer s.Close()
	if _, _, err := asksClientFor(s.URL).PostAsk(context.Background(), "Bug", "b", []string{"bug"}, AskOptions{}); err != nil {
		t.Fatal(err)
	}
	tags := f.hook[0]["applied_tags"].([]any)
	if len(tags) != 2 || tags[0] != "t-bug" || tags[1] != "t-needsme" {
		t.Fatalf("tags = %#v", tags)
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

func TestPostAskInThreadStateTagReplacesNeedsMe(t *testing.T) {
	f := &fakeAsks{}
	s := f.server(t, map[string]Channel{"900": {ID: "900", ParentID: "forum", AppliedTags: []string{"t-task", "t-needsme"}}}, nil)
	defer s.Close()
	if _, err := asksClientFor(s.URL).PostAskInThread(context.Background(), "900", "", "survey", []string{"done"}, AskOptions{}); err != nil {
		t.Fatal(err)
	}
	tags := f.patched["applied_tags"].([]any)
	if len(tags) != 2 || tags[0] != "t-task" || tags[1] != "t-done" {
		t.Fatalf("tags = %#v", tags)
	}
}

func TestPostAskInThreadRejectsMultipleStateTags(t *testing.T) {
	f := &fakeAsks{}
	s := f.server(t, map[string]Channel{"900": {ID: "900", ParentID: "forum", AppliedTags: []string{"t-task", "t-needsme"}}}, nil)
	defer s.Close()
	_, err := asksClientFor(s.URL).PostAskInThread(context.Background(), "900", "", "survey", []string{"done", "working"}, AskOptions{})
	if err == nil || !strings.Contains(err.Error(), "at most one state tag") || f.patched != nil || len(f.hook) != 0 {
		t.Fatalf("err = %v, patched = %#v, posts = %d", err, f.patched, len(f.hook))
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
