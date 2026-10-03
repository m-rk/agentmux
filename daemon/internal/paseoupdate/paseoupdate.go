// Package paseoupdate keeps the Paseo daemon (the @getpaseo/cli npm package)
// current: install the latest stable release, restart the daemon, verify it
// comes back healthy on the new version, and roll back to the previous
// version if it does not. Every external effect (shell commands, restart,
// Discord) is injected through Deps so the flow is unit-testable.
package paseoupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const Package = "@getpaseo/cli"

// Outcome is how an Update run ended.
type Outcome string

const (
	UpToDate       Outcome = "up-to-date"
	Available      Outcome = "available" // check-only: a newer release exists
	Updated        Outcome = "updated"
	RolledBack     Outcome = "rolled-back"
	RollbackFailed Outcome = "rollback-failed"
)

// Deps are the injected effects. Run executes a command as the user that owns
// the paseo install (stdout+stderr combined); Restart restarts the daemon
// service the way this host supervises it.
type Deps struct {
	Run     func(ctx context.Context, name string, args ...string) (string, error)
	Restart func(ctx context.Context) error
	Notify  func(message string) // best effort; nil means log only
	Logf    func(format string, args ...any)
	Sleep   func(time.Duration)

	// HealthTimeout bounds how long the daemon gets to report running on the
	// expected version; Settle is the extra window it must then stay up for,
	// which is what catches a crash-looping service.
	HealthTimeout time.Duration
	Settle        time.Duration
	Poll          time.Duration
}

type Result struct {
	Outcome Outcome
	From    string
	To      string
	Detail  string // failure reason for RolledBack/RollbackFailed
}

var stableVersion = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// Update installs the latest stable release if it is newer than what is
// installed. With checkOnly it only reports. It never downgrades and never
// follows prerelease tags.
func Update(ctx context.Context, d Deps, checkOnly bool) (Result, error) {
	d = d.withDefaults()

	current, err := d.cliVersion(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("reading installed paseo version: %w", err)
	}
	out, err := d.Run(ctx, "npm", "view", Package+"@latest", "version")
	if err != nil {
		return Result{}, fmt.Errorf("looking up latest %s: %w: %s", Package, err, tail(out))
	}
	latest := lastField(out)
	if !stableVersion.MatchString(latest) {
		return Result{}, fmt.Errorf("latest version %q is not a stable x.y.z release; refusing", latest)
	}

	res := Result{From: current, To: latest}
	if !newer(latest, current) {
		res.Outcome = UpToDate
		d.Logf("paseo %s is current (latest %s)", current, latest)
		return res, nil
	}
	if checkOnly {
		res.Outcome = Available
		d.Logf("paseo %s -> %s available", current, latest)
		return res, nil
	}

	d.Logf("updating paseo %s -> %s", current, latest)
	if reason := d.apply(ctx, latest); reason != "" {
		res.Detail = reason
		d.Logf("update to %s failed (%s); rolling back to %s", latest, reason, current)
		if rbReason := d.apply(ctx, current); rbReason != "" {
			res.Outcome = RollbackFailed
			res.Detail = reason + "; rollback also failed: " + rbReason
			d.notify(fmt.Sprintf("🚨 Paseo update %s → %s failed (%s) and the rollback to %s FAILED (%s). Paseo needs manual attention on this host.",
				current, latest, reason, current, rbReason))
			return res, nil
		}
		res.Outcome = RolledBack
		d.notify(fmt.Sprintf("⚠️ Paseo update %s → %s failed (%s). Rolled back to %s and the daemon is healthy again.",
			current, latest, reason, current))
		return res, nil
	}

	res.Outcome = Updated
	d.notify(fmt.Sprintf("✅ Paseo daemon updated %s → %s and is healthy.", current, latest))
	return res, nil
}

// apply installs exactly version, restarts the daemon, and waits for it to be
// healthy on that version. It returns "" on success or a short reason.
func (d Deps) apply(ctx context.Context, version string) string {
	if out, err := d.Run(ctx, "npm", "install", "-g", Package+"@"+version); err != nil {
		return "npm install failed: " + tail(out)
	}
	if got, err := d.cliVersion(ctx); err != nil || got != version {
		return fmt.Sprintf("installed CLI reports %q, want %s", got, version)
	}
	before, _ := d.status(ctx)
	if err := d.Restart(ctx); err != nil {
		return "restart failed: " + err.Error()
	}
	return d.waitHealthy(ctx, version, before)
}

func (d Deps) waitHealthy(ctx context.Context, version string, before daemonStatus) string {
	deadline := time.Now().Add(d.HealthTimeout)
	last := "daemon never reported running"
	for {
		ok, why := d.healthy(ctx, version, before)
		if ok {
			break
		}
		last = why
		if time.Now().After(deadline) || ctx.Err() != nil {
			return last
		}
		d.Sleep(d.Poll)
	}
	d.Sleep(d.Settle)
	if ok, why := d.healthy(ctx, version, before); !ok {
		return "daemon did not stay up: " + why
	}
	return ""
}

type daemonStatus struct {
	LocalDaemon   string `json:"localDaemon"`
	DaemonVersion string `json:"daemonVersion"`
	PID           int    `json:"pid"`
	StartedAt     string `json:"startedAt"`
}

func (d Deps) status(ctx context.Context) (daemonStatus, string) {
	out, err := d.Run(ctx, "paseo", "daemon", "status", "--json")
	if err != nil && out == "" {
		return daemonStatus{}, "status failed: " + err.Error()
	}
	var st daemonStatus
	if jerr := json.Unmarshal([]byte(out), &st); jerr != nil {
		return daemonStatus{}, "unreadable status output"
	}
	return st, ""
}

// healthy reports whether the daemon is running version. The CLI only
// reports daemonVersion when it can authenticate to the daemon, which a
// password-protected daemon refuses without PASEO_PASSWORD. Without it, a
// daemon that started after the restart (new pid or start time) is running
// the package apply just installed and verified, so that counts instead.
func (d Deps) healthy(ctx context.Context, version string, before daemonStatus) (bool, string) {
	st, why := d.status(ctx)
	if why != "" {
		return false, why
	}
	if st.LocalDaemon != "running" {
		return false, "daemon is " + st.LocalDaemon
	}
	if st.DaemonVersion != "" {
		if st.DaemonVersion != version {
			return false, fmt.Sprintf("daemon runs %q, want %s", st.DaemonVersion, version)
		}
		return true, ""
	}
	if st.PID == 0 && st.StartedAt == "" {
		return false, "status reports neither daemonVersion nor pid/startedAt"
	}
	if st.PID == before.PID && st.StartedAt == before.StartedAt {
		return false, fmt.Sprintf("daemon was not restarted (pid %d unchanged)", st.PID)
	}
	return true, ""
}

func (d Deps) cliVersion(ctx context.Context) (string, error) {
	out, err := d.Run(ctx, "paseo", "--version")
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, tail(out))
	}
	v := lastField(out)
	if !stableVersion.MatchString(v) {
		return v, fmt.Errorf("unrecognised version output %q", tail(out))
	}
	return v, nil
}

func (d Deps) notify(message string) {
	d.Logf("%s", message)
	if d.Notify != nil {
		d.Notify(message)
	}
}

func (d Deps) withDefaults() Deps {
	if d.Logf == nil {
		d.Logf = func(string, ...any) {}
	}
	if d.Sleep == nil {
		d.Sleep = time.Sleep
	}
	if d.HealthTimeout == 0 {
		d.HealthTimeout = 90 * time.Second
	}
	if d.Settle == 0 {
		d.Settle = 25 * time.Second
	}
	if d.Poll == 0 {
		d.Poll = 3 * time.Second
	}
	return d
}

// newer reports whether a > b for x.y.z versions.
func newer(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x > y
		}
	}
	return false
}

func lastField(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[len(f)-1]
}

// tail keeps error output short enough for a Discord message.
func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		s = "…" + s[len(s)-300:]
	}
	return s
}
