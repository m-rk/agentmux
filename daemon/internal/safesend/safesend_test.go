package safesend

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const claudeIdle = `⏺ Done. The tests pass.

✻ Crunched for 2m · done 7:25 AM
────────────────────────────────────────
❯
────────────────────────────────────────
  ⏵⏵ auto mode on (shift+tab to cycle)


`

const claudeBusy = `⏺ Running the tests
✳ Swooping… (2m 10s · ↓ 1.0k tokens · thinking)
────────────────────────────────────────
❯
────────────────────────────────────────
  ⏵⏵ auto mode on · 2 shells · esc to interrupt
`

const claudeMenuPrompt = ` Allow this read outside the working directories?
 ❯ 1. Yes, and keep allowing
   2. No
 Esc to cancel · Tab to amend
`

const claudeDraft = `────────────────────────────────────────
❯ make a throwaway vault and try it
────────────────────────────────────────
  ⏵⏵ auto mode on
`

const claudePlaceholder = `────────────────────────────────────────
❯ Try "edit main.go to..."
────────────────────────────────────────
`

// A reply that asks a question in prose, with a draft in the input box.
const claudeQuestionReply = `  TASK-4 is ready to launch. Do you want to:
  - launch it now;
  - or wait?
✻ Cogitated for 7m 50s · done 5:47 PM
────────────────────────────────────────
❯ make a throwaway vault and try it
────────────────────────────────────────
  ⏵⏵ auto mode on (shift+tab to cycle) · PR #1
`

// capture-pane -e of a finished turn showing a greyed prompt suggestion,
// cursor reversed over its first character.
const claudeSuggestion = "⏺ Done. The tests pass.\n" +
	"\x1b[2m────────────────────────────────────────\x1b[0m\n" +
	"❯ \x1b[7mc\x1b[0m\x1b[2mheck the ask and run the smoke test\x1b[0m\n" +
	"\x1b[2m────────────────────────────────────────\x1b[0m\n" +
	"  ⏵⏵ auto mode on (shift+tab to cycle)\n"

// The same pane with the suggestion in grey rather than dim.
var claudeSuggestionGrey = strings.Replace(claudeSuggestion, "\x1b[2mheck", "\x1b[90mheck", 1)

// A real draft with escapes: normal-weight text, cursor after it.
const claudeDraftStyled = "────────────────────────────────────────\n" +
	"❯ make a throwaway vault\x1b[7m \x1b[0m\n" +
	"────────────────────────────────────────\n"

const opencodeBusy = `  ┃  Build · glm custom
  ╹▀▀▀▀▀▀▀▀▀▀
   ⬝■■■■■■⬝  esc interrupt   tab agents  ctrl+p commands
`

const opencodeIdle = `  ┃  Ask anything… "Fix a TODO in the codebase"
  ┃  Build · glm custom
  ╹▀▀▀▀▀▀▀▀▀▀
                       tab agents  ctrl+p commands
`

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name, agent, pane string
		want              State
	}{
		{"claude idle", "claude-code", claudeIdle, StateReady},
		{"claude busy", "claude-code", claudeBusy, StateBusy},
		{"claude menu", "claude-code", claudeMenuPrompt, StatePrompt},
		{"claude draft", "claude-code", claudeDraft, StateDraft},
		{"claude placeholder", "claude-code", claudePlaceholder, StateReady},
		{"claude prose question", "claude-code", claudeQuestionReply, StateDraft},
		{"claude prose question, no draft", "claude-code", strings.Replace(claudeQuestionReply, "❯ make a throwaway vault and try it", "❯ ", 1), StateReady},
		{"claude suggestion", "claude-code", claudeSuggestion, StateReady},
		{"claude grey suggestion", "claude-code", claudeSuggestionGrey, StateReady},
		{"claude suggestion without escapes", "claude-code", ansiSeq.ReplaceAllString(claudeSuggestion, ""), StateDraft},
		{"claude styled draft", "claude-code", claudeDraftStyled, StateDraft},
		{"claude draft after suggestion text", "claude-code", strings.Replace(claudeSuggestion, "\x1b[0m\x1b[2mheck", "\x1b[0mheck", 1), StateDraft},
		{"opencode busy", "opencode", opencodeBusy, StateBusy},
		{"opencode idle", "opencode", opencodeIdle, StateReady},
		{"kilo busy", "kilo", opencodeBusy, StateBusy},
		{"unknown agent", "zero", claudeIdle, StateUnknown},
		// "esc to interrupt" quoted in the conversation, far above the footer.
		{"marker in history", "claude-code", "talk about esc to interrupt\n" + strings.Repeat("x\n", 10) + claudeIdle, StateReady},
	} {
		if got := Classify(tc.agent, tc.pane); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestProvenance(t *testing.T) {
	p := Provenance{Via: "dispatched", By: "orchestrator", From: "TASK-4"}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := p.Compose("do it\nnow"); got != "[dispatched by orchestrator from TASK-4]\ndo it\nnow" {
		t.Fatalf("Compose: %q", got)
	}
	if got := (Provenance{Via: "relayed", By: "orchestrator"}).Prefix(); got != "[relayed by orchestrator]" {
		t.Fatalf("Prefix without from: %q", got)
	}
	for _, bad := range []Provenance{
		{Via: "approved", By: "x"},
		{Via: "relayed", By: ""},
		{Via: "relayed", By: "the user"},
		{Via: "relayed", By: "x", From: "a]b"},
		{Via: "relayed", By: strings.Repeat("a", 65)},
	} {
		if bad.Validate() == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestReasonRetryable(t *testing.T) {
	if !ReasonBusy.Retryable() || ReasonPrompt.Retryable() || ReasonDead.Retryable() || ReasonDraft.Retryable() {
		t.Fatal("retryable set")
	}
}

func TestAuditNeverHoldsText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "send-audit.jsonl")
	const secretText = "please deploy with sk-ant-should-not-appear"
	n, sum := Digest(secretText)
	for i := 0; i < 2; i++ {
		if err := Append(path, AuditEntry{Time: time.Unix(0, 0).UTC(), Principal: "orchestrator", Via: "dispatched", Address: "a@h", Correlation: "TASK-4", Bytes: n, SHA256: sum, Outcome: "delivered"}); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "sk-ant") || strings.Contains(string(data), "deploy") {
		t.Fatal("audit log contains message text")
	}
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	lines := 0
	for sc.Scan() {
		var e AuditEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil || e.SHA256 != sum || e.Correlation != "TASK-4" {
			t.Fatalf("line %d: %+v %v", lines, e, err)
		}
		lines++
	}
	if lines != 2 {
		t.Fatalf("lines = %d", lines)
	}
	info, _ := os.Stat(path)
	dir, _ := os.Stat(filepath.Dir(path))
	if info.Mode().Perm() != 0o600 || dir.Mode().Perm() != 0o700 {
		t.Fatalf("modes %v %v", info.Mode().Perm(), dir.Mode().Perm())
	}
}
