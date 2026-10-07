package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// codexSandboxProblem runs `codex sandbox -- true` as a self-test. It returns
// "" when codex is not installed or the sandbox works. A codex that cannot
// start its bwrap sandbox (typically Ubuntu's AppArmor restriction on
// unprivileged user namespaces) can only talk, so every shell command and
// file write in its read-only/workspace-write modes fails.
func codexSandboxProblem(ctx context.Context) string {
	bin, err := exec.LookPath("codex")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "agentmux-codex-selftest-")
	if err != nil {
		return ""
	}
	defer os.RemoveAll(dir)
	cmd := exec.CommandContext(ctx, bin, "sandbox", "--", "true")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		return ""
	}
	if ctx.Err() != nil {
		return "codex sandbox self-test timed out"
	}
	detail := strings.TrimSpace(string(out))
	if i := strings.LastIndex(detail, "\n"); i >= 0 {
		detail = detail[i+1:]
	}
	return fmt.Sprintf("codex sandbox self-test failed (%v: %s); codex cannot run commands or write files under read-only/workspace-write. See docs/codex-sandbox.md", err, detail)
}
