package collab

import (
	"context"
	"testing"
)

// TestSettleLabelIdempotent pins the trailing-✓ rule: settling an already
// settled label (leading or trailing ✓, any count) leaves exactly one
// trailing check, so the click handler and `asks edit -chosen` commute.
func TestSettleLabelIdempotent(t *testing.T) {
	for in, want := range map[string]string{
		"Ship it":      "Ship it ✓",
		"Ship it ✓":    "Ship it ✓",
		"✓ Ship it":    "Ship it ✓",
		"✓✓ Ship it":   "Ship it ✓",
		"Ship it ✓✓":   "Ship it ✓",
		"✓ Ship it ✓":  "Ship it ✓",
		"✓✓⭐ ▶ Resume": "⭐ ▶ Resume ✓",
		"amp ⭐ ✓ ✓":    "amp ⭐ ✓",
		"amp ⭐ ✔️":     "amp ⭐ ✓",
		"amp ⭐ ✓️ ✓":   "amp ⭐ ✓",
	} {
		if got := settleLabel(in); got != want {
			t.Errorf("settleLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSettleTwiceLeavesOneCheck settles the same button through both paths
// — the gateway click ack and the follow-up edit — and expects one check.
func TestSettleTwiceLeavesOneCheck(t *testing.T) {
	rows := []map[string]any{{"components": []any{
		map[string]any{"type": 2, "style": 2, "label": "Ship it", "custom_id": "ask:Ship it"},
		map[string]any{"type": 2, "style": 2, "label": "Not now", "custom_id": "ask:Not now"},
	}}}
	settleComponents(rows, "ask:Ship it")
	// The edit path reads the clicked message back: label settled, custom id
	// untouched. Re-settling by the original label must match and stay at
	// one trailing check; the settled label form must match too.
	for _, chosen := range []string{"Ship it", "Ship it ✓"} {
		got, err := settleFetchedComponents(rows, chosen)
		if err != nil {
			t.Fatalf("chosen %q: %v", chosen, err)
		}
		btn := got[0]["components"].([]any)[0].(map[string]any)
		if btn["label"] != "Ship it ✓" {
			t.Fatalf("chosen %q: label = %q", chosen, btn["label"])
		}
	}
}

// TestEditKeepsEmbedsSuppressed pins that edits carry the suppress-embeds
// flag by default (so an edit never re-enables cards) and drop it with
// -embeds.
func TestEditKeepsEmbedsSuppressed(t *testing.T) {
	s := &reactEditServer{}
	srv := s.server(t)
	defer srv.Close()
	c := asksClientFor(srv.URL)
	if err := c.EditAsk(context.Background(), "900", "500", EditAskOptions{Chosen: "Ship it"}); err != nil {
		t.Fatal(err)
	}
	if s.bodies[0]["flags"] != float64(4) {
		t.Fatalf("default edit flags = %#v", s.bodies[0]["flags"])
	}
	if err := c.EditAsk(context.Background(), "900", "500", EditAskOptions{Chosen: "Ship it", Embeds: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.bodies[1]["flags"]; ok {
		t.Fatalf("-embeds edit kept flags: %#v", s.bodies[1])
	}
}

// TestSendsSuppressEmbeds pins the flag on every post, reply and edit by
// default, via the fake gateway's recorded payloads.
func TestSendsSuppressEmbeds(t *testing.T) {
	f := &fakeAsks{}
	s := f.server(t, map[string]Channel{"900": {ID: "900", ParentID: "forum", AppliedTags: []string{"t-ask"}}}, nil)
	defer s.Close()
	c := asksClientFor(s.URL)
	if _, _, err := c.PostAsk(context.Background(), "T", "see https://ampcode.com/threads/T-1", nil, AskOptions{}); err != nil {
		t.Fatal(err)
	}
	if f.hook[0]["flags"] != float64(4) {
		t.Fatalf("post flags = %#v", f.hook[0]["flags"])
	}
	if _, err := c.PostAskInThread(context.Background(), "900", "", "see https://ampcode.com/threads/T-1", nil, AskOptions{}); err != nil {
		t.Fatal(err)
	}
	if f.hook[1]["flags"] != float64(4) {
		t.Fatalf("post-in-thread flags = %#v", f.hook[1]["flags"])
	}
	if _, err := c.ReplyAsk(context.Background(), "900", "see https://ampcode.com/threads/T-1", false); err != nil {
		t.Fatal(err)
	}
	if f.hook[2]["flags"] != float64(4) {
		t.Fatalf("reply flags = %#v", f.hook[2]["flags"])
	}
	if _, _, err := c.PostAsk(context.Background(), "T", "b", nil, AskOptions{Embeds: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.hook[3]["flags"]; ok {
		t.Fatalf("-embeds post kept flags: %#v", f.hook[3])
	}
}

// TestSettleChecksOnlySelected pins that re-settling with a different chosen
// button moves the check instead of leaving one on each option.
func TestSettleChecksOnlySelected(t *testing.T) {
	rows := []map[string]any{{"components": []any{
		map[string]any{"type": 2, "style": 2, "label": "amp ⭐", "custom_id": "ask:amp ⭐"},
		map[string]any{"type": 2, "style": 2, "label": "Not now", "custom_id": "ask:Not now"},
	}}}
	settleComponents(rows, "ask:amp ⭐")
	got, err := settleFetchedComponents(rows, "Not now")
	if err != nil {
		t.Fatal(err)
	}
	btns := got[0]["components"].([]any)
	if l := btns[0].(map[string]any)["label"]; l != "amp ⭐" {
		t.Errorf("unselected label = %q, want no check", l)
	}
	if l := btns[1].(map[string]any)["label"]; l != "Not now ✓" {
		t.Errorf("selected label = %q", l)
	}
}
