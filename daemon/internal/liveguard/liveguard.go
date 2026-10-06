// Package liveguard is the one CLI-side check that keeps test traffic out
// of the live Discord and other sessions. Task instances (AGENTMUX_INSTANCE_NAME
// starting with task-, see retire.TaskPrefix) run prompts from other agents,
// so a mistyped `agentmux asks post` inside one can reach a real forum. The
// side-effecting CLI commands therefore refuse when they detect a task
// session, unless a person explicitly opts out with AGENTMUX_ALLOW_LIVE=1 —
// which is never set in a task instance's own environment, only by a person
// or the orchestrator launching the command by hand. See AMUX-32.
package liveguard

import (
	"fmt"
	"os"
	"strings"
)

// TaskPrefix matches retire.TaskPrefix: task sessions are task-*.
const TaskPrefix = "task-"

// AllowEnv is the explicit override. Set by a person or the orchestrator,
// never in task instance env files.
const AllowEnv = "AGENTMUX_ALLOW_LIVE"

// InstanceEnv is the registry variable every provisioned instance carries.
const InstanceEnv = "AGENTMUX_INSTANCE_NAME"

// Refusal is the message refused commands print.
const Refusal = "task sessions can't touch live Discord or other sessions; use fakes or -dry-run"

// IsTaskSession reports whether the current process looks like it runs
// inside a task instance: the instance name starts with task-.
func IsTaskSession() bool {
	return strings.HasPrefix(os.Getenv(InstanceEnv), TaskPrefix)
}

// Allowed reports whether live side effects are permitted: anywhere but a
// task session, or with the explicit override set to 1.
func Allowed() bool {
	if !IsTaskSession() {
		return true
	}
	return os.Getenv(AllowEnv) == "1"
}

// Check returns an error carrying Refusal when live side effects are not
// allowed. Call it at the top of every side-effecting command.
func Check() error {
	if Allowed() {
		return nil
	}
	return fmt.Errorf("%s", Refusal)
}
