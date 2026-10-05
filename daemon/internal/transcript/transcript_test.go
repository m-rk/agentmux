package transcript

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func msgs(n int) []Message {
	out := make([]Message, n)
	for i := range out {
		out[i] = Message{ID: fmt.Sprint(i), Role: RoleUser, Untrusted: true}
	}
	return out
}

func TestPageFromWalksBackwards(t *testing.T) {
	all := msgs(45)
	p, err := PageFrom("t", all, "", 20)
	if err != nil || len(p.Messages) != 20 || p.Messages[0].ID != "25" || p.Messages[19].ID != "44" || p.Older != "25" {
		t.Fatalf("newest page: %+v %v", p, err)
	}
	p, _ = PageFrom("t", all, p.Older, 20)
	if p.Messages[0].ID != "5" || p.Older != "5" {
		t.Fatalf("second page: first=%s older=%q", p.Messages[0].ID, p.Older)
	}
	p, _ = PageFrom("t", all, p.Older, 20)
	if len(p.Messages) != 5 || p.Messages[0].ID != "0" || p.Older != "" {
		t.Fatalf("last page: %d msgs older=%q", len(p.Messages), p.Older)
	}
}

func TestPageFromRejectsBadCursors(t *testing.T) {
	for _, c := range []string{"x", "-1", "46", "01", "3 "} {
		if _, err := PageFrom("t", msgs(45), c, 10); !errors.Is(err, ErrBadCursor) {
			t.Errorf("cursor %q: got %v", c, err)
		}
	}
}

func TestClampLimit(t *testing.T) {
	if ClampLimit(0) != DefaultLimit || ClampLimit(-3) != DefaultLimit || ClampLimit(MaxLimit+1) != MaxLimit || ClampLimit(7) != 7 {
		t.Fatal("ClampLimit bounds")
	}
}

func TestCleanTextRedactsAndCaps(t *testing.T) {
	if got := CleanText("key sk-ant-abc123def456 here"); strings.Contains(got, "sk-ant-abc123") {
		t.Fatalf("not redacted: %q", got)
	}
	long := strings.Repeat("a", MaxTextBytes) + "TAIL"
	got := CleanText("HEAD" + long)
	if len(got) > MaxTextBytes || !strings.HasPrefix(got, "HEAD") || !strings.HasSuffix(got, "TAIL") || !strings.Contains(got, "[truncated]") {
		t.Fatalf("cap: len=%d head=%q tail=%q", len(got), got[:8], got[len(got)-8:])
	}
	if s := CleanSummary(strings.Repeat("é", MaxSummaryBytes)); len(s) > MaxSummaryBytes+len("…") {
		t.Fatalf("summary not capped: %d", len(s))
	}
}

func TestForUnknownAgent(t *testing.T) {
	if _, err := For("no-such-agent"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("got %v", err)
	}
}
