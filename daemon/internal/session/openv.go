package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/m-rk/agentmux/daemon/internal/runas"
)

// An instance can be given secrets without agentmux ever seeing them: drop a
// 1Password env-file at ~/.agentmux/env/<instance>.env containing lines like
//
//	AMP_API_KEY=op://<vault>/<item>/<field>
//
// and RunAmp starts the runner through `op run --env-file=<that file>`, so the
// resolved values exist only in the runner's own environment. The file holds
// references, not secrets. Instances without one launch exactly as before.
//
// The file lives outside the registry deliberately: the provisioner rewrites
// the registry wholesale on every `agentmux new -y`, which would wipe a
// hand-added field, and it needs no daemon/proto plumbing this way.

// opTokenRelPath is the per-host 1Password service account token, relative to
// the run user's home. Without it `op` falls back to the interactive account
// and hangs on a non-tty shell, so it is required, not optional.
const opTokenRelPath = ".config/op/service_account_token"

// opTokenEnv is the variable `op` reads the service account token from.
const opTokenEnv = "OP_SERVICE_ACCOUNT_TOKEN"

// opEnvFilePath is the instance's optional op env-file location.
func opEnvFilePath(name string) string {
	return filepath.Join(runas.CurrentUserHome(), ".agentmux", "env", name+".env")
}

// opEnvFileExists reports whether the instance opted in to op-injected env.
func opEnvFileExists(name string) bool {
	info, err := os.Stat(opEnvFilePath(name))
	return err == nil && info.Mode().IsRegular()
}

// opTokenPath is where the service account token is expected.
func opTokenPath() string {
	return filepath.Join(runas.CurrentUserHome(), opTokenRelPath)
}

// readOpToken returns the service account token, refusing a missing or empty
// file with an error that names the path (never the contents).
func readOpToken() (string, error) {
	path := opTokenPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading 1Password service account token %s: %w", path, err)
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", fmt.Errorf("1Password service account token %s is empty", path)
	}
	return tok, nil
}

// opRunArgs is the argv handed to `op`: resolve envFile, then run the agent
// with the service account token stripped from its environment, so neither
// the runner nor the tools it spawns can read it. Uses /usr/bin/env, present
// at that path on both macOS and Linux.
func opRunArgs(envFile string, agentArgv []string) []string {
	args := []string{"run", "--env-file=" + envFile, "--", "/usr/bin/env", "-u", opTokenEnv}
	return append(args, agentArgv...)
}

// execSyscall is syscall.Exec, replaceable in tests.
var execSyscall = syscall.Exec

// ExecAmp is `agentmux session exec --instance NAME`: the command tmux runs
// for an amp instance that has an op env-file. It replaces itself with
// `op run --env-file=... -- amp <the usual runner flags>`. The token is read
// here and set on the child's environment only — passing it through tmux
// would put it in argv (visible to ps) or in the long-lived tmux server's
// environment (stale across restarts, since the server outlives sessions).
//
// Running this by hand starts a real runner, so it is not a diagnostic.
func ExecAmp(name string) error {
	fields, err := registry(name)
	if err != nil {
		return err
	}
	if agentOf(fields) != "amp" {
		return fmt.Errorf("instance %q is not an amp instance", name)
	}
	envFile := opEnvFilePath(name)
	if !opEnvFileExists(name) {
		return fmt.Errorf("no op env-file at %s", envFile)
	}
	launchArgs, err := ampLaunchArgsFor(name, fields)
	if err != nil {
		return err
	}
	tok, err := readOpToken()
	if err != nil {
		return err
	}

	// withPath resolves op on the run user's PATH and supplies the matching
	// HOME/PATH environment, the same as every other launch here.
	cmd := withPath("op", opRunArgs(envFile, append([]string{"amp"}, launchArgs...))...)
	env := append(cmd.Environ(), opTokenEnv+"="+tok)
	return execSyscall(cmd.Path, cmd.Args, env)
}

// opPreflight checks what ExecAmp will need, so RunAmp fails visibly up front
// instead of starting a tmux session that dies the moment it launches.
func opPreflight() error {
	if _, err := readOpToken(); err != nil {
		return err
	}
	if _, err := runas.CurrentUserLookPath("op"); err != nil {
		return fmt.Errorf("1Password CLI: %w", err)
	}
	return nil
}

// opCommand builds ReadOpRef's op invocation, replaceable in tests.
var opCommand = runas.CurrentUserCommandContext

// ReadOpRef resolves one 1Password secret reference with the service account
// token and returns its value to the caller, for agentmux code that consumes
// a secret itself (thread watch's TypeSafe key) rather than handing it to a
// child. Errors name the reference, never a value; op's stderr is left out
// because nothing here should echo what op printed.
func ReadOpRef(ctx context.Context, ref string) (string, error) {
	tok, err := readOpToken()
	if err != nil {
		return "", err
	}
	cmd := opCommand(ctx, "op", "read", "--no-newline", ref)
	cmd.Env = append(cmd.Environ(), opTokenEnv+"="+tok)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolving %s with op: %w", ref, err)
	}
	value := strings.TrimSpace(string(out))
	if value == "" {
		return "", fmt.Errorf("%s resolved to an empty value", ref)
	}
	return value, nil
}
