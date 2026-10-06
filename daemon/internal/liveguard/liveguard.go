// Package liveguard is the one CLI-side check that keeps test traffic out
// of the live Discord and other sessions. Task instances (names starting
// with task-, see retire.TaskPrefix) run prompts from other agents, so a
// mistyped `agentmux asks post` inside one can reach a real forum. The
// side-effecting CLI commands therefore refuse when they detect a task
// session. The asks commands reroute into the reusable test thread instead
// (see docs/discord-asks.md); every other guarded command just refuses.
//
// There is deliberately no override: a worker that can set an environment
// variable on its own command could also set the override, so an override
// the restrained agent can set itself isn't a guard (see AMUX-39). A real
// boundary means running task sessions as a separate user without the
// Discord token (`sessions run -run-user` exists); note it in the docs.
//
// Identity reaches the guard two ways. `sessions run` marks every amp run
// child with AGENTMUX_INSTANCE_NAME plus AGENTMUX_TASK_SESSION=1 for
// task-* instances (and task claude-code panes get the same pair), so the
// agent's own subprocesses inherit it. When the environment was scrubbed —
// a real amp task run observed on the hub carries neither variable — the
// guard falls back to the working directory: anything inside a
// *-worktrees/task-* path refuses too. See AMUX-34.
package liveguard

import (
	"fmt"
	"os"
	"strings"
)

// TaskPrefix matches retire.TaskPrefix: task sessions are task-*.
const TaskPrefix = "task-"

// InstanceEnv is the registry variable every provisioned instance carries.
// `sessions run` also sets it on every amp run child.
const InstanceEnv = "AGENTMUX_INSTANCE_NAME"

// TaskEnv is the dedicated task-session flag. `sessions run` sets it to
// "1" on amp run children of task-* instances (alongside InstanceEnv),
// and task claude-code panes get the same pair, so the guard fires even
// where the instance name alone would be ambiguous.
const TaskEnv = "AGENTMUX_TASK_SESSION"

// taskWorktreeMarker is the working-directory fallback: task worktrees
// live under a directory ending in -worktrees with a task-* entry inside
// (e.g. ~/agentmux-worktrees/task-AMUX-34). A process whose cwd sits
// under one is inside task work even when its environment was scrubbed.
const taskWorktreeMarker = "-worktrees/task-"

// Refusal is the message refused commands print. It deliberately names no
// override: live posting is for non-task callers only (a person, the
// orchestrator, ask serve), and nothing a task session can set changes
// that.
const Refusal = "task sessions can't touch live Discord or other sessions; use fakes or -dry-run"

// IsTaskSession reports whether the current process looks like it runs
// inside a task instance: the task flag is set, the instance name starts
// with task-, or the working directory sits inside a task worktree.
func IsTaskSession() bool {
	if os.Getenv(TaskEnv) == "1" {
		return true
	}
	if strings.HasPrefix(os.Getenv(InstanceEnv), TaskPrefix) {
		return true
	}
	if wd, err := os.Getwd(); err == nil && InTaskWorktree(wd) {
		return true
	}
	return false
}

// InTaskWorktree reports whether dir sits inside a task worktree path:
// a *-worktrees/task-* segment anywhere in it.
func InTaskWorktree(dir string) bool {
	return strings.Contains(dir, taskWorktreeMarker)
}

// Allowed reports whether live side effects are permitted: anywhere but a
// task session. Task sessions have no override; the asks commands reroute
// into the test thread instead of refusing outright.
func Allowed() bool {
	return !IsTaskSession()
}

// Check returns an error carrying Refusal when live side effects are not
// allowed. Call it at the top of every side-effecting command.
func Check() error {
	if Allowed() {
		return nil
	}
	return fmt.Errorf("%s", Refusal)
}
