package collab

import (
	"strings"
	"testing"
)

func testIdentity() Identity {
	return Identity{Instance: "kilo-minecraft", Host: "build-box.example.net"}
}

func contextItem(thread, from, content string) digestItem {
	return digestItem{
		ThreadID:   thread,
		ThreadName: thread,
		Message:    Message{ID: "1", Content: content, Author: Author{Username: from}},
	}
}

// Found live 2026-09-13: a standby session burned a real model turn on
// every check-in from every other fleet instance, even when nothing in
// the batch was addressed to it — 25 calls over ~35h, all but a handful
// spent reading and dismissing CONTEXT-only broadcasts. buildPrompt's
// wake gate now requires at least one REQUEST (an item addressed to this
// identity) before it returns a non-empty Prompt; CONTEXT-only batches
// are silently absorbed (cursors still advance in BuildDelivery
// regardless of what buildPrompt returns, so nothing is replayed later).
func TestBuildPromptSuppressesContextOnlyBatch(t *testing.T) {
	items := []digestItem{
		contextItem("t1", "webapp-opencode", "Confirmed — working tree clean, no blockers, standing by."),
		contextItem("t2", "agentmux", "Status update: on main, tests pass, no requests addressed to me."),
	}
	got := buildPrompt(SyncOptions{Identity: testIdentity(), Project: "github.com/example/webapp"}, false, items)
	if got != "" {
		t.Fatalf("expected no wake for a context-only batch, got prompt:\n%s", got)
	}
}

func TestBuildPromptWakesOnAddressedItem(t *testing.T) {
	items := []digestItem{
		contextItem("t1", "webapp-opencode", "Confirmed — working tree clean, standing by."),
		contextItem("t2", "agentmux", "@kilo-minecraft@build-box.example.net please rebase on main before merging."),
	}
	got := buildPrompt(SyncOptions{Identity: testIdentity(), Project: "github.com/example/webapp"}, false, items)
	if got == "" {
		t.Fatal("expected a wake when one item addresses this identity, got empty prompt")
	}
	if !strings.Contains(got, "REQUEST in t2") {
		t.Errorf("expected the addressed item labeled REQUEST, got:\n%s", got)
	}
	if !strings.Contains(got, "CONTEXT in t1") {
		t.Errorf("expected the batched context item still included once already woken, got:\n%s", got)
	}
}

func TestBuildPromptOnboardingAlwaysWakesEvenWithNoItems(t *testing.T) {
	got := buildPrompt(SyncOptions{Identity: testIdentity(), Project: "github.com/example/webapp"}, true, nil)
	if got == "" {
		t.Fatal("expected onboarding to always produce a prompt, even with zero items")
	}
}

func TestBuildPromptOnboardingWakesOnContextOnlyBatch(t *testing.T) {
	items := []digestItem{contextItem("t1", "webapp-opencode", "Confirmed — standing by.")}
	got := buildPrompt(SyncOptions{Identity: testIdentity(), Project: "github.com/example/webapp"}, true, items)
	if got == "" {
		t.Fatal("expected onboarding to wake and show context, first contact isn't gated on REQUEST")
	}
	if !strings.Contains(got, "CONTEXT in t1") {
		t.Errorf("expected the context item included during onboarding, got:\n%s", got)
	}
}
