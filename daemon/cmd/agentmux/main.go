// Command agentmux is the agentmux CLI: the TUI by default, plus
// subcommands to manage the per-host daemon and create new instances.
package main

import (
	"fmt"
	"os"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		runTUI(nil)
		return
	}

	switch args[0] {
	case "tui":
		runTUI(args[1:])
	case "daemon":
		runDaemonCmd(args[1:])
	case "new":
		runWizard(args[1:])
	case "session":
		runSessionCmd(args[1:])
	case "-h", "--help", "help":
		printUsage()
	default:
		// Not a known subcommand (e.g. a flag like -socket): fall back to
		// the TUI's own flag parsing so existing invocations still work.
		runTUI(args)
	}
}

func printUsage() {
	fmt.Println(`agentmux: TUI + daemon + instance wizard for agentmux

Usage:
  agentmux                    launch the TUI (default)
  agentmux daemon install     install and start the agentmuxd daemon on this host
  agentmux daemon uninstall   remove the daemon
  agentmux daemon status      check whether the daemon is installed/running
  agentmux daemon run         run the daemon in the foreground (used by the installed unit)
  agentmux new                interactive wizard to create a new instance
  agentmux help                show this message`)
}
