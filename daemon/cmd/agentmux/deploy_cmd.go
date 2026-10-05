package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/m-rk/agentmux/daemon/internal/discovery"
	"github.com/m-rk/agentmux/daemon/internal/gatewayapi"
	"github.com/m-rk/agentmux/daemon/internal/gatewayclient"
	"github.com/m-rk/agentmux/daemon/internal/hostsconfig"
	"github.com/m-rk/agentmux/daemon/internal/ops"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
)

// deployService is one agentmux-owned user service `agentmux deploy`
// restarts after pinning the new binary. unit is the systemd unit name;
// an absent unit is skipped, never an error.
type deployService struct {
	unit  string
	label string
}

// runDeployCmd is `agentmux deploy`: pin the current binary, rewrite the
// daemon unit, restart the daemon and every installed agentmux-owned user
// service, verify each runs the new binary, check no privileged step left
// root-owned files in the template repo, then run the cross-host dispatch
// smoke test. See AMUX-24.
//
// Deploy restarts services but never instances: agentmux-<name>.service
// units keep running through a deploy, so agents are not interrupted.
func runDeployCmd(args []string) {
	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	doctorTime := fs.String("doctor-time", "", "daily doctor time in HH:MM (default: keep the installed timer's time)")
	template := fs.String("template", "", "instance to copy for the smoke test's dry-run create (default: first amp instance on each host)")
	base := fs.String("base", "main", "origin branch the smoke test's dry-run create fetches (default: main)")
	hostsPath := fs.String("hosts", hostsconfig.DefaultPath(), "hosts.yaml listing the fleet (every host is smoke-tested)")
	socketPath := fs.String("socket", daemoninstall.SocketPath(), "Unix socket of the local agentmuxd")
	fs.Parse(args)
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: agentmux deploy [-doctor-time HH:MM] [-template INSTANCE] [-base BRANCH] [-hosts PATH] [-socket PATH]")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	want, err := deployPinBinary()
	if err != nil {
		log.Fatalf("deploy: %v", err)
	}
	fmt.Printf("deploy: pinned %s (%s)\n", want.path, want.sha[:12])

	doctor := *doctorTime
	if doctor == "" {
		doctor, err = deployInstalledDoctorTime(ctx)
		if err != nil {
			log.Fatalf("deploy: %v", err)
		}
		if doctor != "" {
			fmt.Printf("deploy: keeping doctor time %s\n", doctor)
		} else {
			doctor = daemoninstall.DefaultDoctorTime
		}
	}
	if err := daemoninstall.InstallForDeploy(doctor); err != nil {
		log.Fatalf("deploy: %v", err)
	}

	services := deployInstalledServices(ctx)
	names := []string{"agentmuxd.service"}
	for _, s := range services {
		names = append(names, s.unit+" ("+s.label+")")
	}
	userServices := deployInstalledUserServices(ctx, deployRunUser())
	for _, s := range userServices {
		names = append(names, s.unit+" ("+s.label+", user)")
	}
	if runUser := deployRunUser(); runUser != "" && len(userServices) == 0 {
		fmt.Printf("deploy: no installed user units for %s\n", runUser)
	}
	fmt.Printf("deploy: restarting %s\n", strings.Join(names, ", "))
	if err := deployRestartServices(ctx, services); err != nil {
		log.Fatalf("deploy: %v", err)
	}
	if len(userServices) > 0 {
		if err := deployRestartUserServices(ctx, deployRunUser(), userServices); err != nil {
			log.Fatalf("deploy: %v", err)
		}
	}

	if err := deployCheckVersions(ctx, want, services, userServices); err != nil {
		log.Fatalf("deploy: %v", err)
	}
	fmt.Printf("deploy: every service runs %s\n", want.sha[:12])

	if repo := deployTemplateRepo(ctx, *socketPath, *template); repo != "" {
		if err := deployCheckRootOwnedRepos(ctx, repo); err != nil {
			log.Fatalf("deploy: %v", err)
		}
	}

	if err := deploySmokeTest(ctx, *socketPath, *hostsPath, *template, *base); err != nil {
		log.Fatalf("deploy: %v", err)
	}
	fmt.Println("deploy: smoke test passed on every host")
}

// deployPinned is the binary every service must run after a deploy.
type deployPinned struct {
	path string
	sha  string
}

// deployPinBinary copies the running executable to the installed path
// (/usr/local/bin/agentmux on Linux) and hashes it, so the version check
// compares services against what was just installed, not what invoked
// deploy.
func deployPinBinary() (deployPinned, error) {
	self, err := os.Executable()
	if err != nil {
		return deployPinned{}, fmt.Errorf("resolving current executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	dst := deployBinaryPath()
	if self == dst {
		sum, err := deployHashFile(dst)
		if err != nil {
			return deployPinned{}, err
		}
		return deployPinned{path: dst, sha: sum}, nil
	}
	src, err := os.Open(self)
	if err != nil {
		return deployPinned{}, err
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return deployPinned{}, err
	}
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return deployPinned{}, err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		os.Remove(tmp)
		return deployPinned{}, err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return deployPinned{}, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return deployPinned{}, err
	}
	sum, err := deployHashFile(dst)
	if err != nil {
		return deployPinned{}, err
	}
	return deployPinned{path: dst, sha: sum}, nil
}

// deployCheckVersions confirms the daemon and every restarted service run
// the pinned binary, by comparing /proc/<pid>/exe against it. A service
// still on the old binary — the MERG-21 gateway incident — fails the
// deploy instead of silently serving stale logic.
func deployCheckVersions(ctx context.Context, want deployPinned, services, userServices []deployService) error {
	units := []string{"agentmuxd.service"}
	for _, s := range services {
		units = append(units, s.unit)
	}
	var stale []string
	for _, unit := range units {
		pid := deployServiceMainPID(ctx, unit)
		if pid == 0 {
			return fmt.Errorf("%s has no main PID after restart", unit)
		}
		exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
		if err != nil {
			return fmt.Errorf("%s (pid %d): reading exe: %w", unit, pid, err)
		}
		sum, err := deployHashFile(exe)
		if err != nil {
			return fmt.Errorf("%s (pid %d): hashing %s: %w", unit, pid, exe, err)
		}
		mark := "ok " + sum[:12]
		if sum != want.sha {
			mark = "STALE " + sum[:12]
			stale = append(stale, unit)
		}
		fmt.Printf("deploy: %-32s pid %-7d %s\n", unit, pid, mark)
	}
	for _, s := range userServices {
		pid := deployUserServiceMainPID(ctx, deployRunUser(), s.unit)
		if pid == 0 {
			return fmt.Errorf("%s (user) has no main PID after restart", s.unit)
		}
		exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
		if err != nil {
			return fmt.Errorf("%s (user, pid %d): reading exe: %w", s.unit, pid, err)
		}
		sum, err := deployHashFile(exe)
		if err != nil {
			return fmt.Errorf("%s (user, pid %d): hashing %s: %w", s.unit, pid, exe, err)
		}
		mark := "ok " + sum[:12]
		if sum != want.sha {
			mark = "STALE " + sum[:12]
			stale = append(stale, s.unit+" (user)")
		}
		fmt.Printf("deploy: %-32s pid %-7d %s\n", s.unit+" (user)", pid, mark)
	}
	if len(stale) > 0 {
		return fmt.Errorf("still running the old binary: %s (want %s)", strings.Join(stale, ", "), want.sha[:12])
	}
	return nil
}

// deployRunUser is the unprivileged user whose user units and config
// deploy operates on: SUDO_USER when running under sudo, else the current
// user. Empty when no user can be resolved — user units are then skipped.
func deployRunUser() string {
	if u := os.Getenv("SUDO_USER"); u != "" && u != "root" {
		return u
	}
	if u, err := user.Current(); err == nil && u.Username != "" && u.Username != "root" {
		return u.Username
	}
	return ""
}

// deployHostsPath resolves the hosts.yaml deploy smoke-tests: the explicit
// -hosts flag wins, then SUDO_USER's config (sudo runs deploy as root, so
// os.UserHomeDir would point at /root), then the current user's default.
func deployHostsPath(flag string) (path, source string) {
	if flag != "" && flag != hostsconfig.DefaultPath() {
		return flag, "flag -hosts"
	}
	if runUser := deployRunUser(); runUser != "" {
		if u, err := user.Lookup(runUser); err == nil && u.HomeDir != "" {
			p := filepath.Join(u.HomeDir, ".config", "agentmux", "hosts.yaml")
			if _, err := os.Stat(p); err == nil {
				return p, "SUDO_USER " + runUser
			}
			return p, "SUDO_USER " + runUser + " (missing)"
		}
	}
	return hostsconfig.DefaultPath(), "default"
}

// deployInstalledServices returns the owned services whose unit exists on
// this host, so deploy restarts exactly the services the operator set up.
func deployInstalledServices(ctx context.Context) []deployService {
	var out []deployService
	for _, s := range deployOwnedServices() {
		if deployServiceInstalled(ctx, s.unit) {
			out = append(out, s)
		}
	}
	return out
}

// deployTarget is one host the smoke test checks.
type deployTarget struct {
	name    string
	gateway string // empty for the local host
}

// deployTargets returns every host in hosts.yaml (canonicalized and
// de-duplicated) plus the local host when it isn't listed.
func deployTargets(hosts []hostsconfig.Host) []deployTarget {
	var out []deployTarget
	seen := map[string]bool{}
	for _, h := range hosts {
		name := address.Canonical(h.Name)
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, deployTarget{name: name, gateway: h.Gateway})
	}
	if local := address.LocalHostName(); !seen[local] {
		out = append(out, deployTarget{name: local})
	}
	return out
}

// deployTemplateRepo finds the template instance's repository top level,
// for the root-ownership check. Empty when no local template can be
// resolved — the check is skipped, not failed.
func deployTemplateRepo(ctx context.Context, socketPath, template string) string {
	name := template
	if name == "" {
		name = deployDefaultTemplate(ctx, socketPath)
	}
	if name == "" {
		return ""
	}
	fields, err := deployReadRegistry(name)
	if err != nil {
		return ""
	}
	workdir := fields["AGENTMUX_WORKDIR"]
	if workdir == "" {
		return ""
	}
	out, err := exec.CommandContext(ctx, "git", "-C", workdir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// deployReadRegistry reads one instance's registry file.
func deployReadRegistry(name string) (map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(discovery.EnvDir, name+".env"))
	if err != nil {
		return nil, err
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			fields[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return fields, nil
}

// deployDefaultTemplate picks the smoke test's template on this host: the
// first amp instance (its auth is what `sessions run` exercises), else
// the first instance at all. Empty when there are no instances.
func deployDefaultTemplate(ctx context.Context, socketPath string) string {
	sessions, err := ops.Env{SocketPath: socketPath}.List(ctx)
	if err != nil || len(sessions) == 0 {
		return ""
	}
	for _, s := range sessions {
		if s.Agent == "amp" {
			return s.Name
		}
	}
	return sessions[0].Name
}

// deploySmokeTest runs the cross-host dispatch check on every host in
// hosts.yaml plus the local host: a dry-run `sessions create -base` (the
// MERG-20/AMUX-12 sha-versus-branch failure) and a dry-run `sessions
// run` that starts no amp thread (see AMUX-22). Creating real sessions
// or threads would spam the fleet on every deploy; the dry runs exercise
// the same code paths short of the spawn.
func deploySmokeTest(ctx context.Context, socketPath, hostsPath, template, base string) error {
	hostsPath, source := deployHostsPath(hostsPath)
	fmt.Printf("deploy: hosts file %s (from %s)\n", hostsPath, source)
	hosts, err := loadHosts(hostsPath, socketPath)
	if err != nil {
		return fmt.Errorf("loading hosts: %w", err)
	}
	local := address.LocalHostName()
	if template != "" {
		if _, err := address.Parse(template + "@" + local); err != nil {
			return fmt.Errorf("bad -template %q: %w", template, err)
		}
	}

	for _, t := range deployTargets(hosts) {
		tmpl := template
		if tmpl == "" {
			var err error
			tmpl, err = deployHostTemplate(ctx, socketPath, t, local)
			if err != nil {
				return fmt.Errorf("smoke %s: %v", t.name, err)
			}
		}
		if tmpl == "" {
			fmt.Printf("deploy: smoke %-12s skipped (no instances)\n", t.name)
			continue
		}
		tmplAddr := tmpl + "@" + t.name
		if _, err := address.Parse(tmplAddr); err != nil {
			return fmt.Errorf("bad template %q: %w", tmplAddr, err)
		}
		branch := "smoke/deploy-" + time.Now().Format("20060102-150405")
		creq := ops.CreateRequest{
			Template: tmplAddr, Instance: "smoke-deploy",
			Branch: branch, Base: base, DryRun: true,
		}
		cres, err := deployCreateOn(ctx, socketPath, t, local, creq)
		if err != nil {
			e := ops.AsError(err)
			return fmt.Errorf("smoke %s: dry-run create -base %s: %s: %s", t.name, base, e.Reason, e.Detail)
		}
		rreq := ops.RunRequest{
			Address: tmplAddr,
			Text:    "deploy smoke test: reply with exactly: ok",
			DryRun:  true,
		}
		rres, err := deployRunOn(ctx, socketPath, t, local, rreq)
		if err != nil {
			e := ops.AsError(err)
			return fmt.Errorf("smoke %s: dry-run run: %s: %s", t.name, e.Reason, e.Detail)
		}
		fmt.Printf("deploy: smoke %-12s create ok (origin/%s @ %.12s) run ok (%s)\n",
			t.name, cres.Base, cres.BaseCommit, strings.Join(rres.Plan, "; "))
	}
	return nil
}

// deployHostTemplate resolves the smoke template for one host: that host's
// first amp instance, else its first instance. Empty when the host has
// none. Remote hosts are listed through their gateway.
func deployHostTemplate(ctx context.Context, socketPath string, t deployTarget, local string) (string, error) {
	var sessions []ops.Session
	if t.name != local {
		if t.gateway == "" {
			return "", ops.Refuse(safesend.ReasonNotLocal, "host %q has no gateway in hosts.yaml", t.name)
		}
		c := &gatewayclient.Client{BaseURL: t.gateway, HTTP: &http.Client{}, Host: t.name}
		res, err := c.List(ctx)
		if err != nil {
			return "", err
		}
		sessions = res.Sessions
	} else {
		var err error
		sessions, err = ops.Env{SocketPath: socketPath}.List(ctx)
		if err != nil {
			return "", err
		}
	}
	for _, s := range sessions {
		if s.Agent == "amp" {
			return s.Name, nil
		}
	}
	if len(sessions) > 0 {
		return sessions[0].Name, nil
	}
	return "", nil
}

// deployCreateOn runs a create on one host, locally or through its
// gateway.
func deployCreateOn(ctx context.Context, socketPath string, t deployTarget, local string, req ops.CreateRequest) (ops.CreateResult, error) {
	if t.name != local {
		if t.gateway == "" {
			return ops.CreateResult{}, ops.Refuse(safesend.ReasonNotLocal, "host %q has no gateway in hosts.yaml", t.name)
		}
		c := &gatewayclient.Client{BaseURL: t.gateway, HTTP: &http.Client{}, Host: t.name}
		return c.Create(ctx, gatewayapi.CreateRequest{
			Template: req.Template, Instance: req.Instance, Branch: req.Branch,
			Base: req.Base, Worktree: req.Worktree, AllowFiles: req.AllowFiles,
			DryRun: req.DryRun,
		})
	}
	return ops.Env{SocketPath: socketPath}.Create(ctx, req)
}

// deployRunOn runs a run on one host, locally or through its gateway.
func deployRunOn(ctx context.Context, socketPath string, t deployTarget, local string, req ops.RunRequest) (ops.RunResult, error) {
	if t.name != local {
		if t.gateway == "" {
			return ops.RunResult{}, ops.Refuse(safesend.ReasonNotLocal, "host %q has no gateway in hosts.yaml", t.name)
		}
		c := &gatewayclient.Client{BaseURL: t.gateway, HTTP: &http.Client{}, Host: t.name}
		return c.Run(ctx, gatewayapi.RunRequest{
			Address: req.Address, Text: req.Text, Title: req.Title, DryRun: req.DryRun,
		})
	}
	return ops.Env{SocketPath: socketPath}.Run(ctx, req)
}

// deployInstalledDoctorTime reads the installed doctor timer's HH:MM, so
// deploy keeps the operator's schedule instead of resetting it to the
// default. Empty when no doctor timer is installed. Implemented per-OS;
// see deploy_services_linux.go.
func deployInstalledDoctorTime(ctx context.Context) (string, error) {
	return deployDoctorTimeFromTimer(ctx)
}

// deployHashFile hashes a file for the version comparison.
func deployHashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
