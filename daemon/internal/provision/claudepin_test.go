package provision

import "testing"

func TestCleanClaudeModel(t *testing.T) {
	for _, ok := range []string{"", "opus", "sonnet[1m]", "claude-opus-5-5", " claude-sonnet-5-5 "} {
		if _, err := CleanClaudeModel(ok); err != nil {
			t.Errorf("CleanClaudeModel(%q) = %v, want ok", ok, err)
		}
	}
	for _, bad := range []string{"gpt-5", "opus 5", "claude-", "--model", "opus;rm", "Claude-Opus"} {
		if _, err := CleanClaudeModel(bad); err == nil {
			t.Errorf("CleanClaudeModel(%q) accepted", bad)
		}
	}
}

func TestCleanClaudeEffort(t *testing.T) {
	for _, ok := range []string{"", "low", "medium", "high", "xhigh", "max"} {
		if _, err := CleanClaudeEffort(ok); err != nil {
			t.Errorf("CleanClaudeEffort(%q) = %v", ok, err)
		}
	}
	if _, err := CleanClaudeEffort("extreme"); err == nil {
		t.Error("unknown effort accepted")
	}
}

func TestClaudeModelMatches(t *testing.T) {
	cases := []struct {
		pin, actual string
		want        bool
	}{
		{"", "claude-sonnet-5-5", true},
		{"claude-opus-5-5", "", true},
		{"claude-opus-5-5", "claude-opus-5-5", true},
		{"claude-opus-5-5", "claude-opus-5-5-20260101", true},
		{"claude-opus-5-5", "claude-sonnet-5-5", false},
		{"opus", "claude-opus-5-5", true},
		{"opus[1m]", "claude-opus-5-5", true},
		{"opus", "claude-sonnet-5-5", false},
	}
	for _, c := range cases {
		if got := ClaudeModelMatches(c.pin, c.actual); got != c.want {
			t.Errorf("ClaudeModelMatches(%q, %q) = %v, want %v", c.pin, c.actual, got, c.want)
		}
	}
}
