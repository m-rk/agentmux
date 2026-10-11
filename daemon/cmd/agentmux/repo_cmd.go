package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/daemoninstall"
	"github.com/m-rk/agentmux/daemon/internal/hostsconfig"
	"github.com/m-rk/agentmux/daemon/internal/pb"
	"gopkg.in/yaml.v3"
)

var repoSlugRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

type reposConfig struct {
	ReposRoot string `yaml:"repos_root"`
}

func runRepoCmd(args []string) {
	if len(args) == 0 || args[0] != "add" {
		fmt.Fprintln(os.Stderr, "usage: agentmux repo add [-agent amp] [-host local] [-instance NAME] <owner/repo>")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("repo add", flag.ExitOnError)
	hostsPath := fs.String("hosts", hostsconfig.DefaultPath(), "hosts.yaml listing agentmuxd hosts")
	socketPath := fs.String("socket", daemoninstall.SocketPath(), "Unix socket for local agentmuxd")
	host := fs.String("host", "local", "host to create the instance on")
	agent := fs.String("agent", "amp", "agent runner")
	instance := fs.String("instance", "", "instance name (default <repo>-<agent>)")
	rootOverride := fs.String("repos-root", "", "repository root (default from ~/.config/agentmux/repos.yaml or ~/Projects)")
	runUser := fs.String("run-user", "", "Linux OS user running agentmuxd; passed to instance provisioning")
	fs.Parse(args[1:])
	if fs.NArg() != 1 || !repoSlugRE.MatchString(fs.Arg(0)) {
		fmt.Fprintln(os.Stderr, "usage: agentmux repo add [-agent amp] [-host local] [-instance NAME] <owner/repo>")
		os.Exit(2)
	}
	slug := fs.Arg(0)
	if strings.HasSuffix(slug, ".git") {
		slug = strings.TrimSuffix(slug, ".git")
	}
	if strings.Contains(slug, "..") {
		log.Fatalf("repo add: invalid GitHub repository %q", fs.Arg(0))
	}
	if *host != "local" {
		log.Fatalf("repo add: cloning is local to this CLI; run it on the target host (requested %q)", *host)
	}
	if *agent == "" {
		log.Fatal("repo add: -agent cannot be empty")
	}
	root, err := resolveReposRoot(*rootOverride)
	if err != nil {
		log.Fatalf("repo add: %v", err)
	}
	parts := strings.Split(slug, "/")
	workdir := filepath.Join(root, parts[0], parts[1])
	if err := os.MkdirAll(filepath.Dir(workdir), 0o755); err != nil {
		log.Fatalf("repo add: create repo parent: %v", err)
	}
	if st, err := os.Stat(workdir); err == nil {
		if !st.IsDir() {
			log.Fatalf("repo add: %s exists and is not a directory", workdir)
		}
		if _, err := exec.Command("git", "-C", workdir, "rev-parse", "--show-toplevel").Output(); err != nil {
			log.Fatalf("repo add: %s exists but is not a Git checkout", workdir)
		}
		origin, err := exec.Command("git", "-C", workdir, "remote", "get-url", "origin").Output()
		if err != nil || !strings.Contains(strings.ToLower(strings.TrimSpace(string(origin))), strings.ToLower(slug)) {
			log.Fatalf("repo add: %s is a Git checkout, but its origin does not match %s", workdir, slug)
		}
	} else if !os.IsNotExist(err) {
		log.Fatalf("repo add: inspect %s: %v", workdir, err)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		cmd := exec.CommandContext(ctx, "git", "clone", "https://github.com/"+slug+".git", workdir)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		err = cmd.Run()
		cancel()
		if err != nil {
			log.Fatalf("repo add: clone %s: %v", slug, err)
		}
	}
	name := *instance
	if name == "" {
		name = parts[1] + "-" + *agent
	}
	client, err := dialOneHost(*hostsPath, *socketPath, "local")
	if err != nil {
		log.Fatalf("repo add: %v", err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	instances, err := client.ListInstances(ctx)
	cancel()
	if err != nil {
		log.Fatalf("repo add: list instances: %v", err)
	}
	for _, in := range instances {
		if in.Name == name {
			if filepath.Clean(in.Workdir) != filepath.Clean(workdir) || in.Agent != *agent {
				log.Fatalf("repo add: instance %q already exists for agent %s at %s", name, in.Agent, in.Workdir)
			}
			printRepoAdded(slug, name, workdir, true)
			return
		}
	}
	if *runUser == "" {
		if u, err := user.Current(); err == nil {
			*runUser = u.Username
		}
	}
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Minute)
	resp, err := client.CreateInstance(ctx, &pb.CreateInstanceRequest{InstanceName: name, Agent: *agent, Workdir: workdir, RunUser: *runUser})
	cancel()
	if err != nil {
		log.Fatalf("repo add: create instance: %v", err)
	}
	if !resp.Ok {
		log.Fatalf("repo add: %s", resp.Message)
	}
	printRepoAdded(slug, name, workdir, false)
}

func resolveReposRoot(override string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	root := strings.TrimSpace(override)
	if root == "" {
		cfgPath := filepath.Join(home, ".config", "agentmux", "repos.yaml")
		data, err := os.ReadFile(cfgPath)
		if err == nil {
			var cfg reposConfig
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				return "", fmt.Errorf("parse %s: %w", cfgPath, err)
			}
			root = strings.TrimSpace(cfg.ReposRoot)
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("read %s: %w", cfgPath, err)
		}
	}
	if root == "" {
		root = filepath.Join(home, "Projects")
	}
	if root == "~" {
		root = home
	} else if strings.HasPrefix(root, "~/") {
		root = filepath.Join(home, root[2:])
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("repos_root must be an absolute path")
	}
	return filepath.Clean(root), nil
}

func printRepoAdded(slug, instance, workdir string, reused bool) {
	state := "created"
	if reused {
		state = "reused"
	}
	fmt.Printf("Repository: %s\nInstance: %s (%s)\nWorkdir: %s\nWorktrees: %s\n", slug, instance, state, workdir, workdir+"-worktrees")
}
