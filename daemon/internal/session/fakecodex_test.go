package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runFakeCodex(t *testing.T, env []string, args ...string) (string, error) {
	t.Helper()
	fake, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fakecodex", "codex"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(fake, args...)
	cmd.Env = append([]string{"PATH=/usr/bin:/bin"}, env...)
	out, err := cmd.Output()
	return string(out), err
}

func TestFakeCodexScenarios(t *testing.T) {
	cases := []struct {
		scenario string
		wantFail bool
		want     string
	}{
		{"success", false, `"type":"turn.completed"`},
		{"turn_failed", true, `"type":"turn.failed"`},
		{"error_item", false, `"type":"error","message":"synthetic non-fatal`},
		{"rate_limit", true, `rate_limit_error`},
	}
	for _, tc := range cases {
		out, err := runFakeCodex(t, []string{"FAKE_CODEX_SCENARIO=" + tc.scenario}, "exec", "--json", "-s", "read-only", "-m", "m", "-c", "k=v", "-C", t.TempDir(), "hi")
		if (err != nil) != tc.wantFail {
			t.Errorf("%s: err = %v, wantFail %v", tc.scenario, err, tc.wantFail)
		}
		if !strings.Contains(out, tc.want) || !strings.Contains(out, "thread.started") {
			t.Errorf("%s: output %q missing %q", tc.scenario, out, tc.want)
		}
	}
}

func TestFakeCodexResumeKeepsThreadAndWritesOutput(t *testing.T) {
	last := filepath.Join(t.TempDir(), "last")
	id := "00000000-0000-4000-8000-0000000000aa"
	out, err := runFakeCodex(t, nil, "exec", "resume", id, "--json", "-o", last, "go on")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"thread_id":"`+id+`"`) {
		t.Fatalf("resume output %q lacks the resumed thread id", out)
	}
	if data, _ := os.ReadFile(last); !strings.Contains(string(data), "fake codex done") {
		t.Fatalf("-o file = %q", data)
	}
}

func TestFakeCodexRejectsBadInput(t *testing.T) {
	if _, err := runFakeCodex(t, nil, "exec", "-s", "bogus", "x"); err == nil {
		t.Error("bad sandbox accepted")
	}
	if _, err := runFakeCodex(t, nil, "exec", "-C", "/nonexistent-dir", "x"); err == nil {
		t.Error("missing -C dir accepted")
	}
}

func TestFakeCodexHangEmitsStartThenBlocks(t *testing.T) {
	cmd := exec.Command("sh", "-c", `FAKE_CODEX_SCENARIO=hang FAKE_CODEX_HANG_SECONDS=1 ../../testdata/fakecodex/codex exec --json x`)
	out, _ := cmd.Output()
	if !strings.Contains(string(out), "turn.started") || strings.Contains(string(out), "turn.completed") {
		t.Fatalf("hang output = %q", out)
	}
}
