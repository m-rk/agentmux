//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/m-rk/agentmux/daemon/internal/launchagent"
)

// selfUpdateReregisterScript boots the self-update job out and back in after
// a short pause, so the run that started it has finished and exited first.
// A job that starts the agentmux binary directly is pinned to the code
// signature it was bootstrapped against; an update replaces that binary, so
// without this the job fails to start with exit 78 until someone runs
// `self-update install` by hand (AMUX-125, AMUX-132). Booting the job out
// from inside its own run would kill the run, hence a detached shell with
// its own session.
//
// Since AMUX-143 the job starts agentmux through the /bin/sh launcher and is
// not pinned to the binary, so this only runs once, to move a job still
// loaded the old way onto the launcher.
func selfUpdateReregisterScript(uid, plist string) string {
	domain := "gui/" + uid
	return fmt.Sprintf("sleep 5; launchctl bootout %s %s; launchctl bootstrap %s %s",
		domain, shellQuote(plist), domain, shellQuote(plist))
}

func shellQuote(s string) string {
	out := "'"
	for _, r := range s {
		if r == '\'' {
			out += `'\''`
			continue
		}
		out += string(r)
	}
	return out + "'"
}

// shouldReregisterSelfUpdate is true only when this run was started by the
// self-update launchd job itself on macOS: a hand-run `self-update run` has
// no job to replace, and a run under any other label must not touch it.
func shouldReregisterSelfUpdate(goos, xpcServiceName string) bool {
	return goos == "darwin" && xpcServiceName == selfUpdateLabel
}

// selfUpdateJobPinned reports whether the self-update job must be
// re-registered: launchd still spawns the agentmux binary for it directly,
// or `launchctl print` could not say.
func selfUpdateJobPinned(printOut string, printErr error) bool {
	return printErr != nil || launchagent.PinnedToAgentmux(printOut)
}

// prepareSelfUpdatePlist moves the self-update plist onto the launcher if it
// still starts agentmux directly, and reports whether the loaded job needs
// re-registering. A job already loaded through the launcher needs nothing:
// re-registering it would only make macOS report new background activity
// (AMUX-143).
func prepareSelfUpdatePlist(plist string, launchctl launchagent.Launchctl, uid string) (bool, error) {
	content, err := os.ReadFile(plist)
	if err != nil {
		return false, err
	}
	if wrapped, ok := launchagent.Wrap(content); ok {
		if err := launchagent.WriteIfChanged(plist, wrapped); err != nil {
			return false, err
		}
	}
	out, err := launchctl("print", "gui/"+uid+"/"+selfUpdateLabel)
	return selfUpdateJobPinned(out, err), nil
}

// reregisterSelfUpdateJob starts the detached re-register when the job is
// still pinned to the binary. Failure to start it is logged by the caller
// and never fails the update.
func reregisterSelfUpdateJob(home string) error {
	if !shouldReregisterSelfUpdate(runtime.GOOS, os.Getenv("XPC_SERVICE_NAME")) {
		return nil
	}
	plist := filepath.Join(home, "Library", "LaunchAgents", selfUpdateLabel+".plist")
	pinned, err := prepareSelfUpdatePlist(plist, launchagent.Run, selfUpdateUID())
	if err != nil || !pinned {
		return err
	}
	cmd := exec.Command("/bin/sh", "-c", selfUpdateReregisterScript(selfUpdateUID(), plist))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
