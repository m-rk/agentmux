package session

import (
	"fmt"
	"os"
)

// updateAmp runs as the instance's own user already (macOS LaunchAgents are
// per-user; no privilege drop needed) and restarts by calling StopAmp/RunAmp
// directly — see claudecode_darwin.go's updateClaudeCode for why
// re-kickstarting the LaunchAgent instead wouldn't work (RunAmp is
// intentionally idempotent, a no-op against a still-running stale session).
// See amp_linux.go's updateAmp for why `amp update --porcelain` is the
// refresh mechanism rather than an npm install, and for why it still needs
// withNpmGlobalLock: amp update shells out through npm under the hood, and
// every local instance on this host shares one npm global prefix (see
// updateAgent's matching opencode comment in agentmux_darwin.go).
func updateAmp(name string) error {
	fields, err := registry(name)
	if err != nil {
		return err
	}
	session := sessionNameOf(fields, name)
	socket := tmuxSocket(name)

	home, herr := os.UserHomeDir()
	if herr != nil {
		return fmt.Errorf("resolving HOME for npm update lock: %w", herr)
	}
	var out []byte
	var changed, recognized bool
	err = withNpmGlobalLock(home, nil, func() error {
		run := func(name string, args ...string) ([]byte, error) {
			return withPath(name, args...).CombinedOutput()
		}
		var lockErr error
		out, changed, recognized, lockErr = runAmpUpdate(run)
		return lockErr
	})
	if err != nil {
		return fmt.Errorf("amp update failed, leaving existing session running untouched: %w: %s", err, out)
	}
	if !recognized {
		fmt.Printf("warning: %s: could not recognize `amp update --porcelain` output; assuming no version change\n", name)
	}

	after, versionErr := withPath("amp", "--version").CombinedOutput()
	if versionErr != nil {
		return fmt.Errorf("amp reported a successful update but is not runnable afterward, leaving existing session running untouched: %w: %s", versionErr, after)
	}
	if changed {
		fmt.Printf("%s: amp updated to %s\n", name, ampVersionID(string(after)))
	}

	if !changed && hasSession(socket, session) {
		return nil // no version change, session already running
	}
	if err := StopAmp(name); err != nil {
		return fmt.Errorf("stopping %s before restart: %w", name, err)
	}
	return RunAmp(name)
}
