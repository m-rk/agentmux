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
	case "deploy":
		if len(args) > 1 && (args[1] == "begin" || args[1] == "end") {
			runDeployLock(args[1:])
			return
		}
		runDeployCmd(args[1:])
	case "new":
		runWizard(args[1:])
	case "rename":
		runRenameCmd(args[1:])
	case "resume-list":
		runResumeListCmd(args[1:])
	case "session":
		runSessionCmd(args[1:])
	case "list":
		runListCmd(args[1:])
	case "sessions":
		runSessionsCmd(args[1:])
	case "amp":
		runAmpCmd(args[1:])
	case "gc":
		runGCCmd(args[1:])
	case "control":
		runControlCmd(args[1:])
	case "view":
		runViewCmd(args[1:])
	case "send-keys":
		runSendKeysCmd(args[1:])
	case "doctor":
		runDoctorCmd(args[1:])
	case "auth":
		runAuthCmd(args[1:])
	case "notify":
		runNotifyCmd(args[1:])
	case "collab":
		runCollabCmd(args[1:])
	case "asks":
		runAsksCmd(args[1:])
	case "threadwatch":
		runThreadwatchCmd(args[1:])
	case "paseo":
		runPaseoCmd(args[1:])
	case "gateway":
		runGatewayCmd(args[1:])
	case "self-update":
		runSelfUpdateCmd(args[1:])
	case "ship-check":
		runShipCheck(args[1:])
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
  agentmux daemon install [-doctor-time HH:MM]   install the daemon and post-refresh doctor
  agentmux daemon uninstall   remove the daemon
  agentmux daemon status      check whether the daemon is installed/running
  agentmux daemon run         run the daemon in the foreground (used by the installed unit)
  agentmux deploy [-template INSTANCE] [-base BRANCH] [-smoke-name NAME]
                               pin the binary, restart the daemon and every agentmux-owned
                               service, verify versions, and run the cross-host smoke test
  agentmux new                 interactive wizard to create a new instance
  agentmux new -y ...          create an instance non-interactively (see -h)
  agentmux new -y -instance NAME -agent kilo -provider custom -provider-base-url URL -model M
                                update an existing instance's provider/model too (re-run with the same instance+agent)
  agentmux rename ...          rename an instance's tmux session/display name
  agentmux resume-list ...     list resumable Claude Code sessions for a workdir
  agentmux list                headless instance status (name/agent/model/status/workdir); add -json for scripts
  agentmux sessions resolve [-json] INSTANCE@HOST[#THREAD]
                               look up the session an address names (see docs/design/gateway.md)
  agentmux sessions threads|read [-json] INSTANCE@HOST[#THREAD]
                               list a local session's threads, or page through its transcript
  agentmux sessions send -by PRINCIPAL [-json] INSTANCE@HOST[#THREAD] TEXT
                               deliver one message with readiness checks, provenance and audit
  agentmux sessions create -template ADDR -instance NAME -branch B [-base BRANCH] [-worktree NAME] [-allow-file PATH ...]
                               start a task session in a new Git worktree on that host (local or through its gateway)
  agentmux amp sweep [-json] [-dry-run] [-run-user USER]
                               archive junk amp threads that belong to no task instance
  agentmux control ...         start/stop/restart an instance without an attached terminal
  agentmux view -instance NAME        headless read-only snapshot of an instance's tmux pane
  agentmux send-keys -instance NAME KEY...   headless equivalent of typing into an instance's pane
  agentmux doctor              diagnose local sessions and safely recover notable problems
  agentmux auth status [-instance NAME] [-run-user USER] [-all] [-json]
                               check Claude Code login and OAuth refresh-token expiry
  agentmux auth login [-instance NAME] [-run-user USER] [-method claudeai|console] [-force]
                               re-authenticate headlessly: prints a login URL to open on
                               another computer, then pastes the code back
  agentmux notify discord setup    configure the Discord webhook agentmux notifies on (e.g. expiring auth)
  agentmux notify discord setup -y -webhook-url URL   same, non-interactively
  agentmux notify discord test     resend a test message using the saved webhook
  agentmux collab setup            configure Discord forum collaboration
  agentmux collab configure -instance NAME [-project KEY] [-avatar-url URL]
  agentmux collab read -instance NAME [-thread ID]
  agentmux collab post -instance NAME -topic TOPIC -summary SENTENCE [-shared] [-details FILE.md]
  agentmux collab post -instance NAME -thread ID -summary TEXT [-details FILE.md]
  agentmux asks post -title T -body-file F [-tag NAME ...] [-json]   post an ask that mentions the configured user
  agentmux asks reply -thread ID -body-file F [-mention]
  agentmux asks read -thread ID [-after MESSAGE_ID] [-json]
  agentmux asks react -thread ID -message ID -emoji EMOJI [-replace]
  agentmux asks edit -thread ID -message ID [-body-file F] [-disable-buttons] [-chosen LABEL]
  agentmux asks close -thread ID [-tag NAME]
  agentmux asks list [-open|-archived|-all] [-tag NAME] [-since DUR] [-json]
  agentmux asks serve                                          record Discord button clicks for asks
  agentmux threadwatch serve [-dry-run] [-once]   follow agent sessions and alert on Discord (see -h)
  agentmux threadwatch status [-since 24h] [-json]   show open intervene signals
  agentmux threadwatch install -run-user USER     (Linux, root) install agentmux-threadwatch.service
  agentmux threadwatch jev-test [-config PATH]    check the optional TypeSafe key with one synthetic judgment
  agentmux threadwatch review ...                 nightly digest (see its own -h)
  agentmux threadwatch review install [-at 07:00] (Linux, root) install the nightly review timer
  agentmux paseo update [-check]                  update the Paseo daemon to the latest stable release, verify, roll back on failure
  agentmux paseo update install [-at 04:00]       (Linux root / macOS user) schedule that daily
  agentmux gateway run -capability NAME [-listen ADDR]   serve this host's sessions to other tailnet hosts (see docs/gateway.md)
  agentmux gateway install -capability NAME        (Linux root / macOS user) keep that running as a service
  agentmux self-update install|run|status          pull-based updater for the Mac: install up to the shipped commit (see docs/self-update.md)
  agentmux sessions ship|versions|selfupdate-log   publish the shipped commit, or read what a host runs (see docs/self-update.md)
  agentmux ship-check [-base BRANCH] [-workdir PATH] <branch>
                               refuse when the branch carries agent trailers, naming each commit (see docs/gateway.md)
  agentmux deploy begin|end                        hold/release the lock the pull updater skips on
  agentmux help                show this message`)
}
