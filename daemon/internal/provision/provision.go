// Package provision creates new agentmux instances: resolves defaults,
// runs preflight checks, writes the registry file, and installs the
// instance's systemd unit (Linux) or LaunchAgent (macOS). Native Go port
// of backends/*/install.sh and install-macos.sh.
package provision

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/m-rk/agentmux/daemon/internal/discovery"
)

// Options mirrors CreateInstanceRequest; see daemon/proto/agentmuxd.proto.
type Options struct {
	InstanceName    string
	Agent           string
	Provider        string
	Model           string
	Workdir         string
	ResumeSessionID string
	RunUser         string
}

var identifierRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func validateIdentifier(label, value string) error {
	if !identifierRE.MatchString(value) {
		return fmt.Errorf("%s must contain only letters, numbers, dots, underscores, and hyphens", label)
	}
	return nil
}

// Create dispatches to the right agent-specific provisioner. Only
// "claude-code" is implemented so far (Phase B); "zero"/"opencode" land in
// a follow-up phase.
func Create(opts Options) (string, error) {
	switch opts.Agent {
	case "claude-code":
		return createClaudeCode(opts)
	default:
		return "", fmt.Errorf("unsupported agent %q (only claude-code is implemented so far)", opts.Agent)
	}
}

type kv struct{ key, value string }

// writeRegistry writes name.env into discovery.EnvDir in the same
// KEY=VALUE format discovery.go parses, in the given field order —
// matching the bash installers' own cat > file <<EOF field order, so
// output is diffable against them.
func writeRegistry(name string, fields []kv) (string, error) {
	if err := os.MkdirAll(discovery.EnvDir, 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", discovery.EnvDir, err)
	}
	path := filepath.Join(discovery.EnvDir, name+".env")
	var b strings.Builder
	for _, f := range fields {
		fmt.Fprintf(&b, "%s=%s\n", f.key, f.value)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	return path, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// machineName mirrors install.sh's machine_name(): short hostname, minus a
// trailing ".local", falling back to "linux"/"macos".
func machineName(fallback string) string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return fallback
	}
	name = strings.SplitN(name, ".", 2)[0]
	name = strings.TrimSuffix(name, ".local")
	if name == "" {
		return fallback
	}
	return name
}

// realUserCount mirrors install.sh's real_user_count(): counts /etc/passwd
// entries with UID >= 1000, excluding the UID 65534 "nobody" account.
func realUserCount() int {
	data, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return 1
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 3 {
			continue
		}
		var uid int
		if _, err := fmt.Sscanf(fields[2], "%d", &uid); err != nil {
			continue
		}
		if uid >= 1000 && uid != 65534 {
			count++
		}
	}
	return count
}

// displayNameFor mirrors install.sh's default display-name heuristic:
// "<user>:<host> 🤹 <workdir-basename>", with the "<user>:" prefix omitted
// on single-real-user machines.
func displayNameFor(runUser, workdir string) string {
	prefix := ""
	if realUserCount() != 1 {
		prefix = runUser + ":"
	}
	return fmt.Sprintf("%s%s 🤹 %s", prefix, machineName("host"), filepath.Base(workdir))
}
