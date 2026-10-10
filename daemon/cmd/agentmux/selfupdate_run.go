package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/daemoninstall"
	"github.com/m-rk/agentmux/daemon/internal/gatewayclient"
	"github.com/m-rk/agentmux/daemon/internal/hostsconfig"
	"github.com/m-rk/agentmux/daemon/internal/ops"
	"github.com/m-rk/agentmux/daemon/internal/selfupdate"
)

// selfUpdateInstallRepo installs a freshly built binary and records the
// result: atomic install (temp + rename), rollback binary kept, daemon
// restarted, smoke check, versions record updated, one log line.
//
// agentmux: install to the pinned daemon bin, rewrite the daemon and
// gateway plists without loading anything (InstallForDeploy), kickstart
// the daemon and gateway, then run the local smoke check. mergentic:
// install into ~/.local/bin and the agents local bin, no restart.
func selfUpdateInstallRepo(home string, cfg selfUpdateHostConfig, repo, built string) error {
	wantSHA, installed, err := selfUpdateGateState(home, repo)
	if err != nil {
		return err
	}
	switch repo {
	case "agentmux":
		if err := selfUpdateInstallAgentmux(home, cfg, built); err != nil {
			return err
		}
	case "mergentic":
		if err := selfUpdateInstallMergentic(home, cfg, built); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown repo %q", repo)
	}
	gate, err := selfupdate.LoadCommits(selfupdate.VersionsPath(home))
	if err != nil {
		return fmt.Errorf("reading versions: %w", err)
	}
	gate[repo] = wantSHA
	if err := selfupdate.SaveCommits(selfupdate.VersionsPath(home), gate); err != nil {
		return fmt.Errorf("recording version: %w", err)
	}
	_ = installed
	if err := selfupdate.LogEvent(home, selfupdate.DeployedLine(repo, installed, wantSHA)); err != nil {
		return fmt.Errorf("logging deploy: %w", err)
	}
	fmt.Printf("self-update: deployed %s@%s\n", repo, wantSHA)
	return nil
}

// selfUpdateGateState returns the shipped commit to install and the
// installed one it replaces.
func selfUpdateGateState(home, repo string) (wantSHA, installed string, err error) {
	shipped, err := selfupdate.LoadCommits(selfupdate.ShippedPath(home))
	if err != nil {
		return "", "", fmt.Errorf("reading ship gate: %w", err)
	}
	vers, err := selfupdate.LoadCommits(selfupdate.VersionsPath(home))
	if err != nil {
		return "", "", fmt.Errorf("reading versions: %w", err)
	}
	return shipped[repo], vers[repo], nil
}

// installFile copies src to dst atomically (temp + rename) and keeps the
// previous binary as dst.prev for rollback.
func installFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		prev := dst + ".prev"
		if err := copyFile(dst, prev, 0o755); err != nil {
			return fmt.Errorf("keeping rollback binary: %w", err)
		}
	}
	tmp := dst + ".tmp"
	if err := copyFile(src, tmp, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

// rollbackFile puts dst.prev back at dst.
func rollbackFile(dst string) error {
	prev := dst + ".prev"
	if _, err := os.Stat(prev); err != nil {
		return fmt.Errorf("no rollback binary at %s", prev)
	}
	tmp := dst + ".tmp"
	if err := copyFile(prev, tmp, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// selfUpdateInstallAgentmux installs the agentmux binary, restarts the
// daemon and gateway, and smoke-checks both. On smoke failure it rolls
// back to the kept previous binary, restarts, and logs the failure: the
// Mac is never left on a failed build.
func selfUpdateInstallAgentmux(home string, cfg selfUpdateHostConfig, built string) error {
	wantSHA, haveSHA, err := selfUpdateGateState(home, "agentmux")
	if err != nil {
		return err
	}
	bin := cfg.AgentmuxBin
	if err := installFile(built, bin); err != nil {
		return fmt.Errorf("installing agentmux: %w", err)
	}
	restarter := func() error { return selfUpdateRestartAgentmux(bin) }
	if err := restarter(); err != nil {
		_ = rollbackFile(bin)
		_ = restarter()
		return rollbackLogged(home, "agentmux", wantSHA, fmt.Sprintf("restart: %v", err))
	}
	if err := selfUpdateSmoke(cfg); err != nil {
		_ = rollbackFile(bin)
		_ = restarter()
		return rollbackLogged(home, "agentmux", wantSHA, fmt.Sprintf("smoke: %v", err))
	}
	_ = haveSHA
	return nil
}

// selfUpdateRestartAgentmux restarts the daemon and gateway on the new
// binary, exactly once each. Either branch fully reloads the daemon
// (bootout+bootstrap+kickstart), so the caller then only kickstarts the
// gateway — whose plist is never rewritten here, so a kick is enough to
// pick up the new binary bytes at the unchanged path. Failures are
// returned, never fatal here: the caller decides rollback.
func selfUpdateRestartAgentmux(bin string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	doctor := selfUpdateInstalledDoctorTime(homeOf(bin))
	if doctor == "" {
		doctor = daemoninstall.DefaultDoctorTime
	}
	if self != bin {
		// daemon install bootouts+bootstraps+kickstarts the daemon and
		// reloads the doctor/gc jobs; pass the preserved doctor time so
		// a customized schedule survives the update.
		cmd := exec.Command(bin, "daemon", "install", "-doctor-time", doctor)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("daemon install: %w: %s", err, strings.TrimSpace(string(out)))
		}
	} else if err := daemoninstall.Install(doctor); err != nil {
		return fmt.Errorf("reloading daemon: %w", err)
	}
	if out, err := exec.Command("launchctl", "kickstart", "-k", "gui/"+selfUpdateUID()+"/"+gatewayLabel).CombinedOutput(); err != nil {
		return fmt.Errorf("kickstart %s: %w: %s", gatewayLabel, err, strings.TrimSpace(string(out)))
	}
	// Give launchd a moment to respawn before the smoke check probes.
	time.Sleep(5 * time.Second)
	return nil
}

// rollbackLogged logs a failure plus the rollback and returns the error.
func rollbackLogged(home, repo, sha, reason string) error {
	_ = selfupdate.LogEvent(home, selfupdate.FailedLine(repo, sha, reason+" (rolled back)"))
	return fmt.Errorf("%s", reason)
}

// selfUpdateSmoke checks the daemon and gateway answer: the same dry-run
// create+run the hub deploy uses (see deploySmokeTest), pointed at the
// local host. It creates nothing.
func selfUpdateSmoke(cfg selfUpdateHostConfig) error {
	sock := cfg.GatewaySocket
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := selfUpdateSmokeDaemon(ctx, sock); err != nil {
		return err
	}
	return selfUpdateSmokeGateway(ctx, sock)
}

// selfUpdateSmokeDaemon is the daemon half: list answers through the
// local socket.
func selfUpdateSmokeDaemon(ctx context.Context, sock string) error {
	if _, err := (ops.Env{SocketPath: sock}).List(ctx); err != nil {
		return fmt.Errorf("daemon list: %w", err)
	}
	return nil
}

// selfUpdateSmokeGateway is the gateway half: a dry-run create plus a
// dry-run run through the local daemon, the same check deploy runs per
// host (AMUX-27). A forbidden create or run is skipped, not failed, for
// the same reason as in deploy: the grant is deliberately narrow.
func selfUpdateSmokeGateway(ctx context.Context, sock string) error {
	tmpl := deployDefaultTemplate(ctx, sock)
	if tmpl == "" {
		return nil // no instances: nothing to validate against
	}
	branch := "smoke/self-update-" + time.Now().Format("20060102-150405")
	creq, rreq, err := selfUpdateSmokeRequests(tmpl, address.LocalHostName(), branch)
	if err != nil {
		return err
	}
	cres, cerr := (ops.Env{SocketPath: sock}).Create(ctx, creq)
	if cerr != nil && !smokeCreateSkippable(cerr) {
		e := ops.AsError(cerr)
		return fmt.Errorf("dry-run create: %s: %s", e.Reason, e.Detail)
	}
	if _, rerr := (ops.Env{SocketPath: sock}).Run(ctx, rreq); rerr != nil && !smokeRunSkippable(rerr) {
		e := ops.AsError(rerr)
		return fmt.Errorf("dry-run run: %s: %s", e.Reason, e.Detail)
	}
	_ = cres
	return nil
}

func selfUpdateSmokeRequests(template, host, branch string) (ops.CreateRequest, ops.RunRequest, error) {
	templateAddress := template + "@" + host
	smokeAddress := defaultSmokeName + "@" + host
	if _, err := address.Parse(templateAddress); err != nil {
		return ops.CreateRequest{}, ops.RunRequest{}, fmt.Errorf("smoke template address: %w", err)
	}
	if _, err := address.Parse(smokeAddress); err != nil {
		return ops.CreateRequest{}, ops.RunRequest{}, fmt.Errorf("smoke target address: %w", err)
	}
	create := ops.CreateRequest{Template: templateAddress, Instance: defaultSmokeName, Branch: branch, Base: "main", DryRun: true}
	run := ops.RunRequest{Address: smokeAddress, Template: template, Text: "self-update smoke test: reply with exactly: ok", DryRun: true}
	return create, run, nil
}

// selfUpdateInstallMergentic installs the mergentic binary into
// ~/.local/bin and the agents local bin (both atomically, both keeping
// a .prev for rollback). No restart: mergentic is a CLI, not a service.
func selfUpdateInstallMergentic(home string, cfg selfUpdateHostConfig, built string) error {
	dsts := []string{cfg.MergenticBin}
	if cfg.AgentsBinDir != "" {
		dsts = append(dsts, filepath.Join(cfg.AgentsBinDir, "mergentic"))
	}
	for _, dst := range dsts {
		if dst == "" {
			continue
		}
		if err := installFile(built, dst); err != nil {
			_ = selfupdate.LogEvent(home, selfupdate.FailedLine("mergentic", "", err.Error()))
			return fmt.Errorf("installing mergentic to %s: %w", dst, err)
		}
	}
	// Smoke: the installed binary answers --version.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, dsts[0], "--version")
	if out, err := cmd.CombinedOutput(); err != nil {
		for _, dst := range dsts {
			_ = rollbackFile(dst)
		}
		return rollbackLogged(home, "mergentic", "", fmt.Sprintf("version check: %v: %s", err, strings.TrimSpace(string(out))))
	}
	return nil
}

// runSelfUpdateStatus is `agentmux self-update status`: installed vs
// shipped per repo, for the operator (and the orchestrator's lag warn:
// shipped ahead of installed for over 30 minutes).
func runSelfUpdateStatus(args []string) {
	fs := flag.NewFlagSet("self-update status", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print machine-readable JSON")
	fs.Parse(args)
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, selfUpdateUsage)
		os.Exit(2)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("self-update status: %v", err)
	}
	installed, err := selfupdate.LoadCommits(selfupdate.VersionsPath(home))
	if err != nil {
		log.Fatalf("self-update status: %v", err)
	}
	shipped, err := selfupdate.LoadCommits(selfupdate.ShippedPath(home))
	if err != nil {
		log.Fatalf("self-update status: %v", err)
	}
	if *jsonOut {
		writeJSON(map[string]any{"installed": installed, "shipped": shipped})
		return
	}
	for _, repo := range selfupdate.Repos {
		have, want := installed[repo], shipped[repo]
		mark := "up to date"
		if want != "" && want != have {
			mark = "behind shipped " + shortSHA(want)
		} else if want == "" {
			mark = "no shipped commit"
		}
		fmt.Printf("%-10s installed %s shipped %s (%s)\n", repo, shortSHA(have), shortSHA(want), mark)
	}
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	if sha == "" {
		return "-"
	}
	return sha
}

// runSessionsShip is `agentmux sessions ship`: publish the shipped commit
// per repo to a host's gate through its gateway (locally: straight to
// the gate file). The Mac updater installs up to that commit and no
// further. Exit 0 published, 1 refused or failed, 2 usage.
func runSessionsShip(args []string) {
	fs := flag.NewFlagSet("sessions ship", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print machine-readable JSON")
	hostsPath := fs.String("hosts", hostsconfig.DefaultPath(), "hosts.yaml with the gateway URL of other hosts")
	fs.Parse(args)
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentmux sessions ship [-json] [-hosts PATH] <host> [repo@sha ...]")
		os.Exit(2)
	}
	commits, err := parseShipCommits(fs.Args()[1:])
	if err != nil {
		failShip(*jsonOut, fs.Arg(0), "invalid", err.Error())
	}
	host := fs.Arg(0)
	if host == "local" || host == address.LocalHostName() {
		home, err := os.UserHomeDir()
		if err != nil {
			failShip(*jsonOut, host, "failed", err.Error())
		}
		gate, err := publishShipLocal(home, commits)
		if err != nil {
			e := ops.AsError(err)
			failShip(*jsonOut, host, e.Reason, e.Detail)
		}
		printShip(*jsonOut, host, gate)
		return
	}
	cfg, err := hostsconfig.Load(*hostsPath)
	if err != nil {
		failShip(*jsonOut, host, "failed", err.Error())
	}
	var baseURL string
	for _, h := range cfg.Hosts {
		if h.Name == host {
			baseURL = h.Gateway
		}
	}
	if baseURL == "" {
		failShip(*jsonOut, host, "not_found", fmt.Sprintf("host %q has no gateway in %s", host, *hostsPath))
	}
	ctx, cancel := context.WithTimeout(context.Background(), gatewayclient.QueryTimeout)
	defer cancel()
	res, err := (&gatewayclient.Client{BaseURL: baseURL, HTTP: &http.Client{}, Host: host}).ShipPublish(ctx, commits)
	if err != nil {
		e := ops.AsError(err)
		failShip(*jsonOut, host, e.Reason, e.Detail)
	}
	printShip(*jsonOut, host, res.Shipped)
}

// parseShipCommits reads repo@sha args into the gate map.
func parseShipCommits(args []string) (map[string]string, error) {
	out := map[string]string{}
	for _, a := range args {
		repo, sha, ok := strings.Cut(a, "@")
		if !ok || !selfupdate.KnownRepo(repo) || !selfupdate.ValidSHA(sha) {
			return nil, fmt.Errorf("want repo@<40-hex sha>, got %q", a)
		}
		out[repo] = sha
	}
	return out, nil
}

func failShip(jsonOut bool, host string, reason, detail any) {
	if jsonOut {
		writeJSON(map[string]any{"ok": false, "host": host, "reason": reason, "detail": detail})
	} else {
		fmt.Fprintf(os.Stderr, "not shipped %s: %s: %s\n", host, reason, detail)
	}
	os.Exit(1)
}

func printShip(jsonOut bool, host string, gate map[string]string) {
	if jsonOut {
		writeJSON(map[string]any{"ok": true, "host": host, "shipped": gate})
		return
	}
	fmt.Printf("shipped on %s:\n", host)
	for _, repo := range selfupdate.Repos {
		fmt.Printf("  %-10s %s\n", repo, shortSHA(gate[repo]))
	}
}

// homeOf returns the home dir containing bin, or "" when it cannot.
// Used to find the installed doctor plist next to the installed binary.
func homeOf(bin string) string {
	abs, err := filepath.Abs(bin)
	if err != nil {
		return ""
	}
	// <home>/.agentmux/bin/agentmux -> <home>.
	parent := filepath.Dir(filepath.Dir(abs))
	if filepath.Base(filepath.Dir(abs)) == "bin" && filepath.Base(parent) == ".agentmux" {
		return filepath.Dir(parent)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

// selfUpdateInstalledDoctorTime reads the installed doctor plist's
// HH:MM, so a plist refresh keeps the operator's schedule instead of
// resetting it to the default. Empty when no doctor plist is
// installed. The plist is XML, but the hour/minute integers sit on
// their own lines, so a line scan is enough — no XML parsing needed.
func selfUpdateInstalledDoctorTime(home string) string {
	data, err := os.ReadFile(filepath.Join(home, "Library", "LaunchAgents", "com.m-rk.agentmux.doctor.plist"))
	if err != nil {
		return ""
	}
	var hour, minute string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "<key>Hour</key>") {
			hour = ""
		}
		if hour == "" {
			if v, ok := plistInt(line); ok {
				hour = v
			}
			continue
		}
		if minute == "" {
			if v, ok := plistInt(line); ok {
				minute = v
			}
			continue
		}
	}
	if hour == "" || minute == "" {
		return ""
	}
	if len(hour) == 1 {
		hour = "0" + hour
	}
	if len(minute) == 1 {
		minute = "0" + minute
	}
	if _, _, err := daemoninstall.ParseDoctorTime(hour + ":" + minute); err != nil {
		return ""
	}
	return hour + ":" + minute
}

// plistInt reads "<integer>N</integer>" into N.
func plistInt(line string) (string, bool) {
	s, ok := strings.CutPrefix(line, "<integer>")
	if !ok {
		return "", false
	}
	s, ok = strings.CutSuffix(s, "</integer>")
	if !ok {
		return "", false
	}
	return strings.TrimSpace(s), true
}

// selfUpdateUID is the gui domain id for kickstart: this user's uid.
func selfUpdateUID() string {
	if u, err := user.Current(); err == nil {
		return u.Uid
	}
	return "501"
}

// publishShipLocal merges commits into this host's own gate file: the
// local half of `sessions ship`, for the operator publishing by hand on
// the Mac itself.
func publishShipLocal(home string, commits map[string]string) (map[string]string, error) {
	shipped := selfupdate.ShippedPath(home)
	gate, err := selfupdate.LoadCommits(shipped)
	if err != nil {
		return nil, err
	}
	for repo, sha := range commits {
		if !selfupdate.KnownRepo(repo) || !selfupdate.ValidSHA(sha) {
			continue
		}
		gate[repo] = sha
	}
	if err := selfupdate.SaveCommits(shipped, gate); err != nil {
		return nil, err
	}
	return gate, nil
}

// gatewayTailLog is the CLI's local read of the updater event log: the
// last n lines, oldest first. A missing log reads as empty.
func gatewayTailLog(home string, n int) ([]string, error) {
	if n <= 0 {
		n = 50
	}
	if n > 500 {
		n = 500
	}
	data, err := os.ReadFile(selfupdate.LogPath(home))
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return []string{}, nil
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

// runDeployLock is `agentmux deploy begin|end`: hold or release the lock
// file the pull updater skips on. A local session that is mid-deploy
// holds it, so the updater never installs under a deploy in flight.
func runDeployLock(args []string) {
	if len(args) != 1 || (args[0] != "begin" && args[0] != "end") {
		fmt.Fprintln(os.Stderr, "usage: agentmux deploy begin|end")
		os.Exit(2)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("deploy %s: %v", args[0], err)
	}
	lock := selfupdate.LockPath(home)
	switch args[0] {
	case "begin":
		if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
			log.Fatalf("deploy begin: %v", err)
		}
		if err := os.WriteFile(lock, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644); err != nil {
			log.Fatalf("deploy begin: %v", err)
		}
		fmt.Println("deploy lock held; the pull updater will skip until `agentmux deploy end`")
	case "end":
		if err := os.Remove(lock); err != nil && !os.IsNotExist(err) {
			log.Fatalf("deploy end: %v", err)
		}
		fmt.Println("deploy lock released")
	}
}

// runSessionsVersions is `agentmux sessions versions`: show a host's
// installed commits plus the tail of its updater log, so the
// orchestrator can confirm "laptop is on <sha>" or warn when it lags
// by more than 30 minutes. Locally: read the state files straight.
func runSessionsVersions(args []string) {
	fs := flag.NewFlagSet("sessions versions", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print machine-readable JSON")
	hostsPath := fs.String("hosts", hostsconfig.DefaultPath(), "hosts.yaml with the gateway URL of other hosts")
	lines := fs.Int("lines", 10, "updater log lines to show")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: agentmux sessions versions [-json] [-hosts PATH] [-lines N] <host>")
		os.Exit(2)
	}
	host := fs.Arg(0)
	if host == "local" || host == address.LocalHostName() {
		home, err := os.UserHomeDir()
		if err != nil {
			log.Fatalf("sessions versions: %v", err)
		}
		vers, err := selfupdate.LoadCommits(selfupdate.VersionsPath(home))
		if err != nil {
			log.Fatalf("sessions versions: %v", err)
		}
		logLines, err := gatewayTailLog(home, *lines)
		if err != nil {
			log.Fatalf("sessions versions: %v", err)
		}
		printVersions(*jsonOut, host, vers, logLines)
		return
	}
	baseURL := versionsGateway(*hostsPath, host)
	ctx, cancel := context.WithTimeout(context.Background(), gatewayclient.QueryTimeout)
	defer cancel()
	c := &gatewayclient.Client{BaseURL: baseURL, HTTP: &http.Client{}, Host: host}
	vers, err := c.Versions(ctx)
	if err != nil {
		e := ops.AsError(err)
		log.Fatalf("sessions versions: %s: %s", e.Reason, e.Detail)
	}
	logRes, err := c.SelfUpdateLog(ctx, *lines)
	if err != nil {
		e := ops.AsError(err)
		log.Fatalf("sessions versions: %s: %s", e.Reason, e.Detail)
	}
	printVersions(*jsonOut, vers.Host, vers.Versions, logRes.Lines)
}

// runSessionsSelfUpdateLog is `agentmux sessions selfupdate-log`: tail a
// host's pull-updater event log, including the `deployed <repo>@<sha>`
// lines the deploy watch greps for.
func runSessionsSelfUpdateLog(args []string) {
	fs := flag.NewFlagSet("sessions selfupdate-log", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print machine-readable JSON")
	hostsPath := fs.String("hosts", hostsconfig.DefaultPath(), "hosts.yaml with the gateway URL of other hosts")
	lines := fs.Int("lines", 50, "log lines to show (max 500)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: agentmux sessions selfupdate-log [-json] [-hosts PATH] [-lines N] <host>")
		os.Exit(2)
	}
	host := fs.Arg(0)
	if host == "local" || host == address.LocalHostName() {
		home, err := os.UserHomeDir()
		if err != nil {
			log.Fatalf("sessions selfupdate-log: %v", err)
		}
		got, err := gatewayTailLog(home, *lines)
		if err != nil {
			log.Fatalf("sessions selfupdate-log: %v", err)
		}
		printLogLines(*jsonOut, host, got)
		return
	}
	baseURL := versionsGateway(*hostsPath, host)
	ctx, cancel := context.WithTimeout(context.Background(), gatewayclient.QueryTimeout)
	defer cancel()
	res, err := (&gatewayclient.Client{BaseURL: baseURL, HTTP: &http.Client{}, Host: host}).SelfUpdateLog(ctx, *lines)
	if err != nil {
		e := ops.AsError(err)
		log.Fatalf("sessions selfupdate-log: %s: %s", e.Reason, e.Detail)
	}
	printLogLines(*jsonOut, host, res.Lines)
}

// versionsGateway resolves a host's gateway URL from hosts.yaml.
func versionsGateway(hostsPath, host string) string {
	cfg, err := hostsconfig.Load(hostsPath)
	if err != nil {
		log.Fatalf("sessions versions: %v", err)
	}
	for _, h := range cfg.Hosts {
		if h.Name == host {
			if h.Gateway == "" {
				log.Fatalf("sessions versions: host %q has no gateway in %s", host, hostsPath)
			}
			return h.Gateway
		}
	}
	log.Fatalf("sessions versions: host %q not found in %s", host, hostsPath)
	return ""
}

func printVersions(jsonOut bool, host string, vers map[string]string, logLines []string) {
	if jsonOut {
		writeJSON(map[string]any{"ok": true, "host": host, "versions": vers, "log": logLines})
		return
	}
	fmt.Printf("installed on %s:\n", host)
	for _, repo := range selfupdate.Repos {
		fmt.Printf("  %-10s %s\n", repo, shortSHA(vers[repo]))
	}
	for _, l := range logLines {
		fmt.Printf("  | %s\n", l)
	}
}

func printLogLines(jsonOut bool, host string, lines []string) {
	if jsonOut {
		writeJSON(map[string]any{"ok": true, "host": host, "lines": lines})
		return
	}
	for _, l := range lines {
		fmt.Println(l)
	}
}
