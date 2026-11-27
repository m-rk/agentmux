package daemoninstall

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/m-rk/agentmux/daemon/internal/launchagent"
)

const (
	label = "com.agentmux.daemon"
	// Instance labels occupy com.agentmux.<name>; keep the host doctor out of
	// that namespace so an instance named "doctor" cannot collide with it.
	doctorLabel = "com.m-rk.agentmux.doctor"
	// gcLabel is the daily retired-session collection; same namespace
	// reasoning as the doctor (an instance named "gc" must not collide).
	gcLabel = "com.m-rk.agentmux.gc"
	// probeLabel is the hourly provider probe (AMUX-139).
	probeLabel = "com.m-rk.agentmux.probe"
	// gatewayLabel is `agentmux gateway install`'s job (cmd/agentmux).
	gatewayLabel = "com.m-rk.agentmux.gateway"
)

const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
` + launchagent.LauncherXML + `        <string>%s</string>
        <string>daemon</string>
        <string>run</string>
        <string>-socket</string>
        <string>%s</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>%s/agentmuxd.log</string>
    <key>StandardErrorPath</key>
    <string>%s/agentmuxd.err.log</string>
</dict>
</plist>
`

const doctorPlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
` + launchagent.LauncherXML + `        <string>%s</string>
        <string>doctor</string>
    </array>
    <key>StartCalendarInterval</key>
    <dict>
        <key>Hour</key>
        <integer>%d</integer>
        <key>Minute</key>
        <integer>%d</integer>
    </dict>
    <key>ProcessType</key>
    <string>Background</string>
    <key>StandardOutPath</key>
    <string>%s/doctor.log</string>
    <key>StandardErrorPath</key>
    <string>%s/doctor.err.log</string>
</dict>
</plist>
`

const gcPlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
` + launchagent.LauncherXML + `        <string>%s</string>
        <string>gc</string>
        <string>-host</string>
        <string>local</string>
    </array>
    <key>StartCalendarInterval</key>
    <dict>
        <key>Hour</key>
        <integer>4</integer>
        <key>Minute</key>
        <integer>30</integer>
    </dict>
    <key>ProcessType</key>
    <string>Background</string>
    <key>StandardOutPath</key>
    <string>%s/gc.log</string>
    <key>StandardErrorPath</key>
    <string>%s/gc.err.log</string>
</dict>
</plist>
`

// probePlistTemplate runs `agentmux models probe` hourly as the user, like
// the Linux agentmuxd-probe.timer. StartInterval, not a calendar slot, so a
// sleeping Mac catches up once on wake instead of firing every missed hour.
const probePlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
` + launchagent.LauncherXML + `        <string>%s</string>
        <string>models</string>
        <string>probe</string>
    </array>
    <key>StartInterval</key>
    <integer>3600</integer>
    <key>ProcessType</key>
    <string>Background</string>
    <key>StandardOutPath</key>
    <string>%s/probe.log</string>
    <key>StandardErrorPath</key>
    <string>%s/probe.err.log</string>
</dict>
</plist>
`

// agentmuxDir returns ~/.agentmux, the root for everything daemoninstall
// manages on macOS: bin/ (the pinned binary), run/ (the daemon's Unix
// socket), log/ (launchd's stdout/stderr redirection).
func agentmuxDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".agentmux"), nil
}

// SocketPath returns the Unix socket the installed daemon listens on, so
// the TUI/wizard client can default to the right path without a
// hosts.yaml (see install_linux.go's SocketPath for the Linux default).
// Falls back to a relative path if the home directory can't be resolved,
// matching defaultEnvDir's documented failure behavior.
func SocketPath() string {
	dir, err := agentmuxDir()
	if err != nil {
		return ".agentmux/run/agentmuxd.sock"
	}
	return filepath.Join(dir, "run", "agentmuxd.sock")
}

func plistPath() (string, error) {
	return launchAgentPath(label)
}

func doctorPlistPath() (string, error) {
	return launchAgentPath(doctorLabel)
}

func gcPlistPath() (string, error) {
	return launchAgentPath(gcLabel)
}

func probePlistPath() (string, error) {
	return launchAgentPath(probeLabel)
}

func launchAgentPath(agentLabel string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", agentLabel+".plist"), nil
}

// Install renders and loads a per-user LaunchAgent, pointing it at a stable
// copy of the current binary under ~/.agentmux/bin. Unlike Linux, this must
// NOT run as root: instances on macOS are user-level LaunchAgents (see
// backends/*/install-macos.sh, which explicitly refuse sudo), so agentmuxd
// itself stays in the same unprivileged, per-user world — no root needed
// anywhere in the macOS path.
//
// Every plist starts the binary through the /bin/sh launcher, and only a
// plist whose content changed is booted out and back in (AMUX-143): a
// re-registration is what macOS reports as new background activity. On an
// ordinary update nothing is re-registered; the daemon and gateway are
// kickstarted onto the new binary and the scheduled jobs pick it up on
// their next run.
func Install(doctorTime string) error {
	plists, err := pinAndRender(doctorTime)
	if err != nil {
		return err
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	for _, p := range plists {
		if _, err := launchagent.Apply(launchctl, domain, p.path, p.content); err != nil {
			return err
		}
	}
	if err := runCmd("launchctl", "kickstart", "-k", domain+"/"+label); err != nil {
		return err
	}
	gatewayPlist, err := launchAgentPath(gatewayLabel)
	if err != nil {
		return err
	}
	if err := refreshGatewayAgent(domain, gatewayPlist); err != nil {
		return err
	}
	// Every other agentmux job still starting the binary directly moves onto
	// the launcher, once: instance jobs, their .update jobs and the
	// self-update job (AMUX-125, AMUX-143).
	plist, err := plistPath()
	if err != nil {
		return err
	}
	migrated, err := migrateAgentJobs(domain, filepath.Dir(plist), map[string]bool{
		label: true, doctorLabel: true, gcLabel: true, probeLabel: true, gatewayLabel: true,
	})
	if len(migrated) > 0 {
		fmt.Printf("Moved %d other launchd jobs onto the launcher\n", len(migrated))
	}
	if err != nil {
		return err
	}

	dir, err := agentmuxDir()
	if err != nil {
		return err
	}
	sock := filepath.Join(dir, "run", "agentmuxd.sock")
	bin := filepath.Join(dir, "bin", "agentmux")
	fmt.Printf("Installed and started %s plus %s at %s local time plus %s and %s (binary: %s, socket: %s)\n", label, doctorLabel, doctorTime, gcLabel, probeLabel, bin, sock)
	return nil
}

// agentPlist is one rendered LaunchAgent and where it goes.
type agentPlist struct {
	path    string
	content []byte
}

// InstallForDeploy pins the current binary under ~/.agentmux/bin and
// rewrites the daemon, doctor, gc and probe plists without loading
// anything, so `agentmux deploy` can refresh every unit before restarting
// all the services itself (see AMUX-24). doctorTime is still parsed, so a
// bad value fails the same way Install would. An unchanged plist is not
// rewritten.
func InstallForDeploy(doctorTime string) error {
	plists, err := pinAndRender(doctorTime)
	if err != nil {
		return err
	}
	for _, p := range plists {
		if err := launchagent.WriteIfChanged(p.path, p.content); err != nil {
			return err
		}
	}
	return nil
}

// pinAndRender pins the current binary under ~/.agentmux/bin and renders
// the daemon, doctor, gc and probe plists, daemon first, without writing
// them.
func pinAndRender(doctorTime string) ([]agentPlist, error) {
	if os.Geteuid() == 0 {
		return nil, fmt.Errorf("must not be run as root/sudo on macOS; run as your normal user")
	}

	dir, err := agentmuxDir()
	if err != nil {
		return nil, err
	}
	doctorHour, doctorMinute, err := ParseDoctorTime(doctorTime)
	if err != nil {
		return nil, err
	}
	for _, sub := range []string{"bin", "run", "log"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, err
		}
	}

	bin := filepath.Join(dir, "bin", "agentmux")
	if err := installSelf(bin); err != nil {
		return nil, fmt.Errorf("installing binary to %s: %w", bin, err)
	}

	sock := filepath.Join(dir, "run", "agentmuxd.sock")
	logDir := filepath.Join(dir, "log")
	var plists []agentPlist
	for _, r := range []struct {
		path    func() (string, error)
		content string
	}{
		{plistPath, fmt.Sprintf(plistTemplate, label, bin, sock, logDir, logDir)},
		{doctorPlistPath, fmt.Sprintf(doctorPlistTemplate, doctorLabel, bin, doctorHour, doctorMinute, logDir, logDir)},
		{gcPlistPath, fmt.Sprintf(gcPlistTemplate, gcLabel, bin, logDir, logDir)},
		{probePlistPath, fmt.Sprintf(probePlistTemplate, probeLabel, bin, logDir, logDir)},
	} {
		path, err := r.path()
		if err != nil {
			return nil, err
		}
		plists = append(plists, agentPlist{path, []byte(r.content)})
	}
	return plists, nil
}

func Uninstall() error {
	plist, err := plistPath()
	if err != nil {
		return err
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	doctorPlist, doctorErr := doctorPlistPath()
	if doctorErr != nil {
		return doctorErr
	}
	gcPlist, gcErr := gcPlistPath()
	if gcErr != nil {
		return gcErr
	}
	probePlist, probeErr := probePlistPath()
	if probeErr != nil {
		return probeErr
	}
	_ = runCmd("launchctl", "bootout", domain, probePlist)
	_ = runCmd("launchctl", "bootout", domain, gcPlist)
	_ = runCmd("launchctl", "bootout", domain, doctorPlist)
	_ = runCmd("launchctl", "bootout", domain, plist)
	for _, path := range []string{probePlist, gcPlist, doctorPlist, plist} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing %s: %w", path, err)
		}
	}
	fmt.Printf("Removed %s\n", label)
	return nil
}

func Status() (string, error) {
	plist, err := plistPath()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(plist); os.IsNotExist(err) {
		return fmt.Sprintf("not installed (no %s)", plist), nil
	}
	dir, _ := agentmuxDir()
	sock := filepath.Join(dir, "run", "agentmuxd.sock")
	domain := "gui/" + strconv.Itoa(os.Getuid())
	out := captureCmd("launchctl", "print", domain+"/"+label)
	doctorPlist, _ := doctorPlistPath()
	doctorOut := captureCmd("launchctl", "print", domain+"/"+doctorLabel)
	gcPlist, _ := gcPlistPath()
	gcOut := captureCmd("launchctl", "print", domain+"/"+gcLabel)
	probePlist, _ := probePlistPath()
	probeOut := captureCmd("launchctl", "print", domain+"/"+probeLabel)
	return fmt.Sprintf("plist: %s\nsocket: %s\n\n%s\n\ndoctor plist: %s\n%s\n\ngc plist: %s\n%s\n\nprobe plist: %s\n%s", plist, sock, out, doctorPlist, doctorOut, gcPlist, gcOut, probePlist, probeOut), nil
}
