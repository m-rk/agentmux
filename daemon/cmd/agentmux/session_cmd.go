package main

import "fmt"

// runSessionCmd is `agentmux session run|update --instance NAME`: the
// per-instance unit's ExecStart, replacing rc-start.sh/rc-update.sh.
// Placeholder until the session package lands.
func runSessionCmd(args []string) {
	fmt.Println("agentmux session: not implemented yet")
}
