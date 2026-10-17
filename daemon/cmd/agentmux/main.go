// Command agentmux is the agentmux CLI: the TUI by default, plus
// subcommands to manage the per-host daemon and create new instances.
package main

import (
	"encoding/json"
	"fmt"
	"github.com/m-rk/agentmux/daemon/internal/modelsource"
	"io"
	"os"
	"runtime/debug"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		runTUI(nil)
		return
	}

	switch args[0] {
	case "__models-probe":
		os.Exit(modelsource.ProbeHelper(args[1:]))
	case "version", "-version", "--version":
		runVersion(args[1:])
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
	case "repo":
		runRepoCmd(args[1:])
	case "rename":
		runRenameCmd(args[1:])
	case "remove":
		runRemoveCmd(args[1:])
	case "resume-list":
		runResumeListCmd(args[1:])
	case "session":
		runSessionCmd(args[1:])
	case "list":
		runListCmd(args[1:])
	case "sessions":
		runSessionsCmd(args[1:])
	case "taskcheck-broker":
		runTaskcheckBroker(args[1:])
	case "amp":
		runAmpCmd(args[1:])
	case "models":
		runModelsCmd(args[1:])
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
	case "discord":
		runDiscordCmd(args[1:])
	case "asks":
		if info, err := os.Stderr.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
			printAsksAliasNotice(os.Stderr, true)
		}
		runDiscordCmd(args[1:])
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

type versionInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// Set by release/build commands with -ldflags "-X main.commit=<sha>".
var commit = "unknown"

func currentVersion() versionInfo {
	v := versionInfo{Version: "devel", Commit: commit}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if bi.Main.Version != "" {
			v.Version = bi.Main.Version
		}
		for _, setting := range bi.Settings {
			if setting.Key == "vcs.revision" && setting.Value != "" {
				v.Commit = setting.Value
			}
		}
	}
	return v
}

func runVersion(args []string) {
	jsonOutput := false
	for _, arg := range args {
		if arg == "-json" || arg == "--json" {
			jsonOutput = true
		}
	}
	v := currentVersion()
	if jsonOutput {
		_ = json.NewEncoder(os.Stdout).Encode(v)
		return
	}
	fmt.Printf("agentmux %s (%s)\n", v.Version, v.Commit)
}

func printUsage() {
	fmt.Println(`agentmux: TUI + daemon + instance wizard for agentmux

Usage:
  agentmux version [-json]     print the installed version and commit
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
  agentmux repo add OWNER/REPO [-agent amp] [-host local]  clone and provision an instance
  agentmux new -y -instance NAME -agent kilo -provider custom -provider-base-url URL -model M
                                update an existing instance's provider/model too (re-run with the same instance+agent)
  agentmux rename ...          rename an instance's tmux session/display name
  agentmux remove -instance NAME  stop, disable, and archive an instance
  agentmux resume-list ...     list resumable Claude Code sessions for a workdir
  agentmux list                headless instance status; partial host failures warn on stderr; add -json for scripts
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
  agentmux models list [-json] [-config PATH]
                               check configured model source availability
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
  agentmux collab doctor           check forum, webhook, tags, and bot permissions
  agentmux collab configure -instance NAME [-project KEY] [-avatar-url URL]
  agentmux collab read -instance NAME [-thread ID]
  agentmux collab post -instance NAME -topic TOPIC -summary SENTENCE [-shared] [-details FILE.md]
  agentmux collab post -instance NAME -thread ID -summary TEXT [-details FILE.md]
  agentmux discord post -title T -body-file F [-tag NAME ...] [-no-mention] [-json] post an ask (mentions the configured user by default)
  agentmux discord reply -thread ID -body-file F [-mention]
  agentmux discord read -thread ID [-after MESSAGE_ID] [-json]
  agentmux discord read -threads ID1,ID2 [-after-map JSON] -json
  agentmux discord heads -threads ID1,ID2 -json
  agentmux discord react -thread ID -message ID -emoji EMOJI [-replace]
  agentmux discord edit -thread ID -message ID [-body-file F] [-buttons-json FILE] [-disable-buttons] [-chosen LABEL]
  agentmux discord pin THREAD MESSAGE
  agentmux discord close -thread ID [-tag NAME]
  agentmux discord list [-open|-archived|-all] [-tag NAME] [-since DUR] [-json]
  agentmux discord serve                                        record Discord button clicks
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

func printAsksAliasNotice(w io.Writer, terminal bool) {
	if terminal {
		fmt.Fprintln(w, "asks is now agentmux discord; the alias will be removed")
	}
}
