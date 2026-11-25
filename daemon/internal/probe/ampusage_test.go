package probe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/m-rk/agentmux/daemon/internal/catalogue"
)

const ampSignedIn = "Signed in as someone@example.com (someone)\n"

func TestParseAmpUsage(t *testing.T) {
	cases := []struct {
		name, out string
		ok        bool
		kind      string
	}{
		{"credit", ampSignedIn + "**Individual credits:** $21.34 remaining (set up auto-reload) - https://ampcode.com/settings\n**Workspace w:** $0.00 remaining - https://ampcode.com/workspaces/w\n", true, ""},
		{"workspace only", ampSignedIn + "**Individual credits:** $0.00 remaining\n**Workspace w:** $1,250.00 remaining\n", true, ""},
		{"zero", ampSignedIn + "**Individual credits:** $0.00 remaining\n", false, catalogue.ErrKindQuota},
		{"negative", ampSignedIn + "**Individual credits:** -$3.10 remaining\n**Workspace w:** $-1.00 remaining\n", false, catalogue.ErrKindQuota},
		{"no credit line", ampSignedIn, false, catalogue.ErrKindQuota},
		{"signed out", "**Individual credits:** $5.00 remaining\n", false, catalogue.ErrKindAuth},
		{"empty", "", false, catalogue.ErrKindAuth},
	}
	for _, c := range cases {
		a := Parse(catalogue.RunnerAmp, []byte(c.out))
		if (a.Err == nil) != c.ok || !a.Tokens.Zero() || a.Session != "" {
			t.Errorf("%s: %+v", c.name, a)
			continue
		}
		if c.ok {
			continue
		}
		f := Judge(a.Err, nil, false, "")
		if f.Kind != c.kind || strings.Contains(f.Words, "example") || strings.Contains(f.Words, "someone") {
			t.Errorf("%s: judged %+v", c.name, f)
		}
	}
	// amp exits 1 on stderr when it has no key.
	a := Parse(catalogue.RunnerAmp, nil)
	if f := Judge(a.Err, errors.New("exit status 1"), false, "Error: Invalid or missing API key. Run 'amp login' to authenticate."); f.Kind != catalogue.ErrKindAuth {
		t.Errorf("no key: %+v", f)
	}
}

func TestAmpArgsMakeNoThread(t *testing.T) {
	if got := Args(Target{Runner: catalogue.RunnerAmp, Model: "rush"}); len(got) != 1 || got[0] != "usage" {
		t.Errorf("amp args = %q", got)
	}
}

func TestRetireAmpThread(t *testing.T) {
	path := AmpThreadPath(t.TempDir())
	var archived []string
	var logs []string
	ok := func(_ context.Context, id string) error { archived = append(archived, id); return nil }
	log := func(l string) { logs = append(logs, l) }

	RetireAmpThread(context.Background(), path, ok, log) // no file: nothing to do
	if len(archived) != 0 || len(logs) != 0 {
		t.Fatalf("no file: archived %v logs %v", archived, logs)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"id":"T-1","started_at":"2026-11-25T00:00:00Z","turns":13}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fail := func(context.Context, string) error { return errors.New("signed out") }
	RetireAmpThread(context.Background(), path, fail, log)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a failed archive must keep the file: %v", err)
	}
	RetireAmpThread(context.Background(), path, ok, log)
	if len(archived) != 1 || archived[0] != "T-1" {
		t.Fatalf("archived %v", archived)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file should be gone: %v", err)
	}
	RetireAmpThread(context.Background(), path, ok, log) // once only
	if len(archived) != 1 || len(logs) != 2 {
		t.Fatalf("archived %v logs %v", archived, logs)
	}
}
