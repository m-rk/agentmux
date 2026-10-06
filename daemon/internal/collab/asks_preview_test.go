package collab

import (
	"strings"
	"testing"
)

func previewClient() *Client {
	c := testClient("http://127.0.0.1:1")
	c.Config.AskMentionUserID = "777"
	return c
}

var previewTags = []string{"task", "needs me", "mergentic", "done"}

func TestPreviewPostPrintsPayload(t *testing.T) {
	p, err := previewClient().PreviewPost("Launch X?", "Please decide", []string{"mergentic"}, AskOptions{}, previewTags)
	if err != nil {
		t.Fatalf("PreviewPost: %v", err)
	}
	if p.Kind != "post" || p.Title != "Launch X?" {
		t.Fatalf("preview = %+v", p)
	}
	if !strings.HasPrefix(p.Content, "<@777>\n") {
		t.Fatalf("content = %q, want the mention first", p.Content)
	}
	out := p.Format()
	for _, want := range []string{"dry-run: would send post", "title: Launch X?", "<@777>", "tags: task, mergentic, needs me"} {
		if !strings.Contains(out, want) {
			t.Fatalf("formatted preview missing %q:\n%s", want, out)
		}
	}
	js, err := p.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, want := range []string{`"kind": "post"`, `"mention_user_id": "777"`, `@777`} {
		if !strings.Contains(js, want) {
			t.Fatalf("JSON preview missing %q:\n%s", want, js)
		}
	}
}

func TestPreviewPostUnknownTag(t *testing.T) {
	if _, err := previewClient().PreviewPost("T", "body", []string{"nope"}, AskOptions{}, previewTags); err == nil {
		t.Fatal("want an error for an unknown tag, got nil")
	}
}

func TestPreviewReplyMention(t *testing.T) {
	p, err := previewClient().PreviewReply("123", "done", true)
	if err != nil {
		t.Fatalf("PreviewReply: %v", err)
	}
	if !strings.HasPrefix(p.Content, "<@777>\n") || p.ThreadID != "123" {
		t.Fatalf("preview = %+v", p)
	}
	if out := p.Format(); !strings.Contains(out, "would send reply to thread 123") {
		t.Fatalf("formatted reply missing the thread:\n%s", out)
	}
}

func TestPreviewReplyNoMention(t *testing.T) {
	p, err := previewClient().PreviewReply("123", "done", false)
	if err != nil {
		t.Fatalf("PreviewReply: %v", err)
	}
	if strings.Contains(p.Content, "<@") {
		t.Fatalf("content = %q, want no mention", p.Content)
	}
}
