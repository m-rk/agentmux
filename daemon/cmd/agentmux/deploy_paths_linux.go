//go:build linux

package main

// deployBinaryPath is the installed binary every service execs: the same
// path daemon install pins (see daemoninstall's binPath).
func deployBinaryPath() string { return "/usr/local/bin/agentmux" }
