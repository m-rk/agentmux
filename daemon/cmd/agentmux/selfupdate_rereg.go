//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
)

// selfUpdateReregisterScript boots the self-update job out and back in after
// a short pause, so the run that started it has finished and exited first.
// launchd pins a job to the code signature of the binary it was bootstrapped
// against; an update replaces that binary, so without this the job fails to
// start with exit 78 until someone runs `self-update install` by hand
// (AMUX-125, AMUX-132). Booting the job out from inside its own run would
// kill the run, hence a detached shell with its own session.
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

// reregisterSelfUpdateJob starts the detached re-register. Failure to start
// it is logged by the caller and never fails the update.
func reregisterSelfUpdateJob(home string) error {
	if !shouldReregisterSelfUpdate(runtime.GOOS, os.Getenv("XPC_SERVICE_NAME")) {
		return nil
	}
	plist := filepath.Join(home, "Library", "LaunchAgents", selfUpdateLabel+".plist")
	if _, err := os.Stat(plist); err != nil {
		return err
	}
	cmd := exec.Command("/bin/sh", "-c", selfUpdateReregisterScript(selfUpdateUID(), plist))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
