package session

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// loginPathTimeout bounds the login-shell probe: a profile that hangs must
// not stall starting an instance.
const loginPathTimeout = 10 * time.Second

// hostLoginPath returns the PATH the host user's login shell ends up with
// (so the Go toolchain, ripgrep and friends installed by their profile are
// found), or "" when it cannot be determined. Replaceable in tests.
var hostLoginPath = func() string {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	ctx, cancel := context.WithTimeout(context.Background(), loginPathTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, shell, "-lc", `printf %s "$PATH"`).Output()
	if err != nil {
		return ""
	}
	// A noisy profile may print before the PATH; the PATH is the last line.
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// AmpWorkerPath is the PATH an amp task instance starts with: first the
// directory of the task wrappers (when given), then the host user's login
// shell PATH, then whatever PATH this process already has. The unit that
// creates the tmux session runs with a minimal PATH, so without the login
// part workers hit "go: command not found" and have to discover an
// `export PATH=...` prefix on every call. Duplicates are dropped.
func AmpWorkerPath(stubDir string) string {
	var parts []string
	if stubDir != "" {
		parts = append(parts, stubDir)
	}
	parts = append(parts, strings.Split(hostLoginPath(), ":")...)
	parts = append(parts, strings.Split(os.Getenv("PATH"), ":")...)
	seen := map[string]bool{}
	var out []string
	for _, p := range parts {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return strings.Join(out, ":")
}

// ampToolRun runs one toolchain probe with exactly the given environment
// and reports whether it exited zero. Replaceable in tests with a fake that
// records the environment it was handed.
var ampToolRun = func(env []string, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), loginPathTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	return cmd.Run()
}

// ampToolProbes are the commands a worker needs on PATH.
var ampToolProbes = [][]string{{"go", "version"}, {"rg", "--version"}}

// CheckAmpToolchain runs `go version` and `rg --version` with the PATH an
// amp task instance gets and returns the names that failed. The probe
// binary is resolved against that same PATH, not the caller's.
func CheckAmpToolchain() []string {
	path := AmpWorkerPath("")
	env := append(os.Environ(), "PATH="+path)
	var missing []string
	for _, p := range ampToolProbes {
		bin := p[0]
		if full := lookPathIn(path, bin); full != "" {
			bin = full
		}
		if err := ampToolRun(env, bin, p[1:]...); err != nil {
			missing = append(missing, p[0])
		}
	}
	return missing
}

// AmpToolchainWarning is the one-line warning for CheckAmpToolchain's
// result, or "" when nothing is missing.
func AmpToolchainWarning(instance string, missing []string) string {
	if len(missing) == 0 {
		return ""
	}
	return fmt.Sprintf("%s: %s not found on the instance's PATH (the host user's login-shell PATH); workers will need an export PATH prefix", instance, strings.Join(missing, " and "))
}

func lookPathIn(path, name string) string {
	for _, d := range strings.Split(path, ":") {
		if d == "" {
			continue
		}
		p := d + "/" + name
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}
