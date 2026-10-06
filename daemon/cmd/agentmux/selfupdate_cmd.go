package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"context"

	"github.com/m-rk/agentmux/daemon/internal/daemoninstall"
	"github.com/m-rk/agentmux/daemon/internal/liveguard"
	"github.com/m-rk/agentmux/daemon/internal/selfupdate"
)

// selfUpdateHostConfig is everything personal about the Mac host, kept
// out of the repo: repo URLs to fetch, checkout dirs to keep, install
// destinations. `self-update install` takes them as flags (or env, for
// the URLs the operator pastes once) and bakes them into the LaunchAgent;
// `self-update run` reads them back from its own environment.
type selfUpdateHostConfig struct {
	AgentmuxURL   string // git URL to fetch agentmux from
	MergenticURL  string // git URL to fetch mergentic from
	AgentmuxDir   string // checkout dir for agentmux (a mirror the updater fetches)
	MergenticDir  string // checkout dir for mergentic
	AgentmuxBin   string // where the built agentmux binary installs (pinned daemon bin)
	MergenticBin  string // where the built mergentic binary installs (~/.local/bin)
	AgentsBinDir  string // extra dir receiving the mergentic binary (the agents local bin)
	GatewaySocket string // daemon socket for the post-install smoke check
}

// selfUpdateUsage is the command's help.
const selfUpdateUsage = `usage:
  agentmux self-update install [-bin PATH] [-print]
        [-agentmux-url URL] [-mergentic-url URL]
        [-agentmux-dir PATH] [-mergentic-dir PATH]
        [-agentmux-bin PATH] [-mergentic-bin PATH] [-agents-bin-dir PATH]
                               install the every-10-minutes pull updater (macOS user LaunchAgent)
  agentmux self-update run      fetch origin/main and install up to the shipped commit (run by launchd)
  agentmux self-update status   show the installed vs shipped commits per repo
  agentmux deploy begin|end     hold/release the lock the updater skips on (a mid-deploy session)`

// runSelfUpdateCmd is `agentmux self-update`: the pull-based updater for
// the Mac (AMUX-29). `install` (once, by hand) writes the launchd job;
// `run` (every 10 minutes, by launchd) pulls and installs. `status`
// answers "which commit does this host run" for the operator.
func runSelfUpdateCmd(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, selfUpdateUsage)
		os.Exit(2)
	}
	switch args[0] {
	case "install":
		runSelfUpdateInstall(args[1:])
	case "run":
		runSelfUpdateRun(args[1:])
	case "status":
		runSelfUpdateStatus(args[1:])
	default:
		fmt.Fprintln(os.Stderr, selfUpdateUsage)
		os.Exit(2)
	}
}

// selfUpdateConfigFromFlags builds the host config from flags over env
// over defaults. Defaults are generic (no personal paths, hostnames or
// usernames): the operator passes the real checkout dirs and URLs once
// at install time.
func selfUpdateConfigFromFlags(fs *flag.FlagSet) selfUpdateHostConfig {
	home, _ := os.UserHomeDir()
	cfg := selfUpdateHostConfig{
		AgentmuxURL:   os.Getenv("AGENTMUX_SELF_UPDATE_AGENTMUX_URL"),
		MergenticURL:  os.Getenv("AGENTMUX_SELF_UPDATE_MERGENTIC_URL"),
		AgentmuxDir:   os.Getenv("AGENTMUX_SELF_UPDATE_AGENTMUX_DIR"),
		MergenticDir:  os.Getenv("AGENTMUX_SELF_UPDATE_MERGENTIC_DIR"),
		AgentmuxBin:   os.Getenv("AGENTMUX_SELF_UPDATE_AGENTMUX_BIN"),
		MergenticBin:  os.Getenv("AGENTMUX_SELF_UPDATE_MERGENTIC_BIN"),
		AgentsBinDir:  os.Getenv("AGENTMUX_SELF_UPDATE_AGENTS_BIN_DIR"),
		GatewaySocket: os.Getenv("AGENTMUX_SELF_UPDATE_SOCKET"),
	}
	fs.StringVar(&cfg.AgentmuxURL, "agentmux-url", cfg.AgentmuxURL, "git URL to fetch agentmux from")
	fs.StringVar(&cfg.MergenticURL, "mergentic-url", cfg.MergenticURL, "git URL to fetch mergentic from")
	fs.StringVar(&cfg.AgentmuxDir, "agentmux-dir", cfg.AgentmuxDir, "checkout dir for agentmux")
	fs.StringVar(&cfg.MergenticDir, "mergentic-dir", cfg.MergenticDir, "checkout dir for mergentic")
	fs.StringVar(&cfg.AgentmuxBin, "agentmux-bin", cfg.AgentmuxBin, "where the built agentmux binary installs")
	fs.StringVar(&cfg.MergenticBin, "mergentic-bin", cfg.MergenticBin, "where the built mergentic binary installs")
	fs.StringVar(&cfg.AgentsBinDir, "agents-bin-dir", cfg.AgentsBinDir, "extra dir receiving the mergentic binary")
	fs.StringVar(&cfg.GatewaySocket, "socket", cfg.GatewaySocket, "daemon socket for the post-install smoke check")
	if cfg.AgentmuxDir == "" && home != "" {
		cfg.AgentmuxDir = filepath.Join(home, "src", "agentmux")
	}
	if cfg.MergenticDir == "" && home != "" {
		cfg.MergenticDir = filepath.Join(home, "src", "mergentic")
	}
	if cfg.AgentmuxBin == "" && home != "" {
		cfg.AgentmuxBin = filepath.Join(home, ".agentmux", "bin", "agentmux")
	}
	if cfg.MergenticBin == "" && home != "" {
		cfg.MergenticBin = filepath.Join(home, ".local", "bin", "mergentic")
	}
	if cfg.GatewaySocket == "" {
		cfg.GatewaySocket = daemoninstall.SocketPath()
	}
	return cfg
}

// runSelfUpdateInstall is `agentmux self-update install`: write and load
// the launchd job. Installing rewrites host services: never from a task
// session (see liveguard).
func runSelfUpdateInstall(args []string) {
	if err := liveguard.Check(); err != nil {
		log.Fatalf("self-update install: %v", err)
	}
	fs := flag.NewFlagSet("self-update install", flag.ExitOnError)
	bin := fs.String("bin", "", "agentmux binary path the job execs (default: the daemon's pinned binary)")
	print := fs.Bool("print", false, "print the plist without installing it")
	cfg := selfUpdateConfigFromFlags(fs)
	fs.Parse(args)
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, selfUpdateUsage)
		os.Exit(2)
	}
	if cfg.AgentmuxURL == "" || cfg.MergenticURL == "" {
		log.Fatal("self-update install: -agentmux-url and -mergentic-url are required (or AGENTMUX_SELF_UPDATE_*_URL)")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("self-update install: %v", err)
	}
	binPath := *bin
	if binPath == "" {
		binPath = cfg.AgentmuxBin
	}
	if err := installSelfUpdateSchedule(home, binPath, cfg, *print); err != nil {
		log.Fatalf("self-update install: %v", err)
	}
}

// selfUpdateRepo describes one repo for a run: its gate name, where to
// fetch, and how to build and install it.
type selfUpdateRepo struct {
	name string // agentmux | mergentic
	url  string // git URL to fetch
	dir  string // checkout the updater fetches into
}

// runSelfUpdateRun is `agentmux self-update run`: fetch origin/main for
// each repo and install up to the shipped commit when it moved. One repo
// failing never stops the other: each logs its own line.
func runSelfUpdateRun(args []string) {
	fs := flag.NewFlagSet("self-update run", flag.ExitOnError)
	cfg := selfUpdateConfigFromFlags(fs)
	fs.Parse(args)
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, selfUpdateUsage)
		os.Exit(2)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("self-update run: %v", err)
	}
	if selfupdate.Locked(home) {
		log.Printf("self-update run: a local session is mid-deploy; skipping")
		return
	}
	shipped, err := selfupdate.LoadCommits(selfupdate.ShippedPath(home))
	if err != nil {
		log.Fatalf("self-update run: %v", err)
	}
	installed, err := selfupdate.LoadCommits(selfupdate.VersionsPath(home))
	if err != nil {
		log.Fatalf("self-update run: %v", err)
	}
	repos := []selfUpdateRepo{
		{name: "agentmux", url: cfg.AgentmuxURL, dir: cfg.AgentmuxDir},
		{name: "mergentic", url: cfg.MergenticURL, dir: cfg.MergenticDir},
	}
	for _, r := range repos {
		if r.url == "" || r.dir == "" {
			continue
		}
		if err := selfUpdateOneRepo(home, cfg, r, shipped[r.name], installed[r.name]); err != nil {
			log.Printf("self-update run: %s: %v", r.name, err)
		}
	}
	installed, err = selfupdate.LoadCommits(selfupdate.VersionsPath(home))
	if err != nil {
		log.Fatalf("self-update run: %v", err)
	}
	_ = installed
}

// selfUpdateOneRepo fetches origin/main for one repo and installs the
// shipped commit when the installed one lags it.
func selfUpdateOneRepo(home string, cfg selfUpdateHostConfig, r selfUpdateRepo, wantSHA, haveSHA string) error {
	if !selfupdate.WantInstall(r.name, haveSHA, wantSHA) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	run := func(dir string, name string, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return string(out), nil
	}
	if _, err := os.Stat(filepath.Join(r.dir, ".git")); err != nil {
		if _, err := run("", "git", "clone", r.url, r.dir); err != nil {
			return recordFail(home, r.name, wantSHA, fmt.Sprintf("clone: %v", err))
		}
	}
	if _, err := run(r.dir, "git", "fetch", "origin", "main"); err != nil {
		return recordFail(home, r.name, wantSHA, fmt.Sprintf("fetch: %v", err))
	}
	// Safety: install only commits on origin/main. merge-base --is-ancestor
	// proves wantSHA is reachable from origin/main (i.e. it really is on
	// main, not some other ref the gate was tricked into naming).
	if _, err := run(r.dir, "git", "merge-base", "--is-ancestor", wantSHA, "origin/main"); err != nil {
		return recordFail(home, r.name, wantSHA, fmt.Sprintf("not on origin/main: %v", err))
	}
	workdir, err := os.MkdirTemp("", "self-update-"+r.name+"-")
	if err != nil {
		return recordFail(home, r.name, wantSHA, err.Error())
	}
	defer os.RemoveAll(workdir)
	if _, err := run(r.dir, "git", "worktree", "add", "--detach", workdir, wantSHA); err != nil {
		return recordFail(home, r.name, wantSHA, fmt.Sprintf("worktree: %v", err))
	}
	defer run(r.dir, "git", "worktree", "remove", "--force", workdir)
	if out, err := run(workdir, "go", "vet", "./..."); err != nil {
		return recordFail(home, r.name, wantSHA, fmt.Sprintf("go vet: %v: %s", err, strings.TrimSpace(out)))
	}
	bin, err := selfUpdateBuild(r.name, cfg, workdir, run)
	if err != nil {
		return recordFail(home, r.name, wantSHA, err.Error())
	}
	if err := selfUpdateInstallRepo(home, cfg, r.name, bin); err != nil {
		return recordFail(home, r.name, wantSHA, err.Error())
	}
	return nil
}

// selfUpdateBuild compiles repo in a temporary worktree and returns the
// built binary path. agentmux builds ./cmd/agentmux; mergentic builds
// ./cmd/mergentic.
func selfUpdateBuild(repo string, cfg selfUpdateHostConfig, workdir string, run func(dir, name string, args ...string) (string, error)) (string, error) {
	pkg := "./cmd/agentmux"
	if repo == "mergentic" {
		pkg = "./cmd/mergentic"
	}
	out := filepath.Join(workdir, "self-update-build", repo)
	if out, err := run(workdir, "go", "build", "-o", out, pkg); err != nil {
		return "", fmt.Errorf("go build %s: %w: %s", pkg, err, strings.TrimSpace(out))
	}
	_ = cfg
	return filepath.Join(workdir, "self-update-build", repo), nil
}

// recordFail logs a failure line and returns it as the error.
func recordFail(home, repo, sha, reason string) error {
	_ = selfupdate.LogEvent(home, selfupdate.FailedLine(repo, sha, reason))
	return fmt.Errorf("%s", reason)
}
