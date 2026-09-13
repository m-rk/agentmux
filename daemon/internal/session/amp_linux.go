package session

import (
	"fmt"
	"os/exec"
)

// updateAmp runs as root (it needs to call systemctl), dropping to the
// instance's run user via runas for the amp calls themselves — the same
// split updateAgentmux and updateClaudeCode use.
//
// The CLI is refreshed with `amp update`, amp's own documented self-update,
// rather than `npm install -g @sourcegraph/amp`. That is the opposite of the
// choice made for opencode (see updateAgent's comment in agentmux_linux.go,
// where shelling through the agent's own binary meant a broken install could
// never repair itself) and is deliberate: amp publishes a --porcelain
// contract for exactly this use, and `amp update` is the only mechanism that
// works regardless of how amp was installed — the npm package is a thin
// wrapper whose postinstall merely hardlinks a platform binary into place, so
// the npm route would be wrong for a curl-installed amp. The unrepairable-
// install failure mode is still real; it surfaces as the explicit error
// below rather than being silently papered over.
func updateAmp(name string) error {
	fields, err := registry(name)
	if err != nil {
		return err
	}
	runUser := fields["AGENTMUX_RUN_USER"]
	if runUser == "" {
		return fmt.Errorf("registry for %s is missing AGENTMUX_RUN_USER", name)
	}
	serviceName := "agentmux-" + name + ".service"
	session := sessionNameOf(fields, name)
	socket := tmuxSocket(name)

	out, err := runAs(runUser, "amp", "update", "--porcelain").CombinedOutput()
	if err != nil {
		return fmt.Errorf("amp update failed, leaving existing session running untouched: %w: %s", err, out)
	}
	changed, recognized := ampUpdateChanged(string(out))
	if !recognized {
		// Not an error: amp exited 0, so the update itself is fine. Treated
		// as "no change" (see ampUpdateChanged) and logged so a wording
		// change in amp's porcelain output is visible in the journal rather
		// than silently disabling update-triggered restarts forever.
		fmt.Printf("warning: %s: could not recognize `amp update --porcelain` output; assuming no version change\n", name)
	}

	after, versionErr := runAs(runUser, "amp", "--version").CombinedOutput()
	if versionErr != nil {
		return fmt.Errorf("amp reported a successful update but is not runnable afterward, leaving existing session running untouched: %w: %s", versionErr, after)
	}
	if changed {
		fmt.Printf("%s: amp updated to %s\n", name, ampVersionID(string(after)))
	}

	if !changed && hasSessionAs(runUser, socket, session) {
		return nil // no version change, session already running
	}
	if err := exec.Command("systemctl", "restart", serviceName).Run(); err != nil {
		return fmt.Errorf("restarting %s: %w", serviceName, err)
	}
	return nil
}
