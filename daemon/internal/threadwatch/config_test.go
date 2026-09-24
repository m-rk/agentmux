package threadwatch

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	for _, in := range []string{
		"key sgamp_abcdef123456 here",
		"export API_KEY=abcdefgh12345678",
		"Authorization: Bearer abcdefghijklmnop",
	} {
		out := Redact(in)
		if !strings.Contains(out, "[redacted]") || strings.Contains(out, "abcdef") {
			t.Errorf("Redact(%q) = %q", in, out)
		}
	}
	if got := Redact("nothing secret"); got != "nothing secret" {
		t.Errorf("clean text changed: %q", got)
	}
}

func TestExcerptKeepsTail(t *testing.T) {
	s := strings.Repeat("é", MaxExcerptBytes) + "END"
	out := Excerpt(s)
	if !strings.HasSuffix(out, "END") || len(out) > MaxExcerptBytes+len("…") {
		t.Fatalf("bad excerpt len %d", len(out))
	}
}
