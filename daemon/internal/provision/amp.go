package provision

import (
	"fmt"
	"os/exec"
	"strings"
)

const defaultAmpInstance = "amp"

// maxAmpRunnerIDLen is the DNS label limit. amp's own docs only say
// "Runner IDs must be valid hostnames and are case-insensitive. Amp
// preserves the casing you provide." — no length is stated, and the
// installed CLI (0.0.1789300838-gde32db) accepts any string locally and
// only validates server-side once authenticated, so the limit that a
// "valid hostname" implies is applied here rather than discovered.
const maxAmpRunnerIDLen = 63

// AmpRunnerID converts an agentmux instance name into the value passed to
// `amp --runner-id`. agentmux's own validateIdentifier accepts dots and
// underscores (`[A-Za-z0-9._-]+`), neither of which belongs in a single DNS
// label: an underscore is not a legal hostname character at all, and a dot
// would silently turn one instance name into a multi-label name. So this is
// deliberately stricter than validateIdentifier rather than reusing it —
// see amp's runner docs (ampcode.com/docs/cli/runners) for the hostname
// requirement.
//
// The result is lowercased (amp treats IDs case-insensitively, so folding
// the case costs nothing and makes the value stable), has every character
// outside [a-z0-9-] replaced with a hyphen, collapses runs of hyphens, and
// is trimmed of leading/trailing hyphens — a label may not start or end
// with one. It is idempotent: feeding an already-valid ID back through it
// returns the same string, which is what lets callers sanitize a registry
// value they didn't compute themselves without having to tell the two cases
// apart.
//
// An input that sanitizes away to nothing is an error rather than a
// silently-invented fallback: a runner ID is how a human finds this machine
// in ampcode.com's runner list, so quietly substituting something
// unrecognizable would be worse than refusing.
func AmpRunnerID(instance string) (string, error) {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(instance)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			// Everything else — '.', '_', and anything a hand-edited
			// registry might hold — becomes a hyphen, collapsed below.
			b.WriteByte('-')
		}
	}
	id := b.String()
	for strings.Contains(id, "--") {
		id = strings.ReplaceAll(id, "--", "-")
	}
	id = strings.Trim(id, "-")
	if len(id) > maxAmpRunnerIDLen {
		// Trim again: truncation can leave a trailing hyphen behind.
		id = strings.Trim(id[:maxAmpRunnerIDLen], "-")
	}
	if id == "" {
		return "", fmt.Errorf("instance name %q contains no characters usable in an amp runner ID (which must be a valid hostname)", instance)
	}
	return id, nil
}

// rejectUnsupportedAmpOptions refuses the provider-family knobs on an amp
// instance instead of silently ignoring them. amp's headless runner takes
// its model and account entirely from the signed-in Amp account
// (`amp --no-tui --runner-id <id> --remote-control-terminal` has no
// provider/model/base-URL/API-key arguments at all), and amp has no
// --resume equivalent, so a caller passing any of these has misunderstood
// something and deserves to be told rather than to watch the setting
// vanish. This is amp's counterpart to validateSupportedAgentProvider,
// which only guards the zero/opencode/kilo family that actually has a
// provider to validate.
func rejectUnsupportedAmpOptions(opts Options) error {
	for _, unsupported := range []struct{ flag, value string }{
		{"-provider", opts.Provider},
		{"-model", opts.Model},
		{"-provider-base-url", opts.BaseURL},
		{"-provider-api-key-env", opts.APIKeyEnv},
		{"-resume", opts.ResumeSessionID},
		{"-compact", opts.CompactOnUpdate},
	} {
		if strings.TrimSpace(unsupported.value) != "" {
			return fmt.Errorf("%s is not supported for the amp agent (amp's headless runner takes its account and model from the signed-in Amp account, and has no resume/compact concept)", unsupported.flag)
		}
	}
	return nil
}

// ampAuthProblemVia reports why amp isn't usable for the target user, or ""
// when it is. The command is built per-platform (a privilege-dropped
// runas.Command on Linux, a same-user runas.CurrentUserCommand on macOS),
// mirroring how claudeLoggedInVia is shared between the two claude-code
// provisioners.
//
// `amp usage` is the probe because it is the only auth-dependent amp
// subcommand confirmed (against 0.0.1789300838-gde32db) to fail *cleanly*
// when no API key is stored: it exits 1 with "Invalid or missing API key.
// Run 'amp login' to authenticate." Most other subcommands — `amp threads
// list`, `amp tools list`, and crucially `amp --no-tui` itself — instead
// start an interactive device-code login flow and block forever waiting for
// a human. That is exactly the failure this preflight exists to prevent: an
// unauthenticated amp instance would otherwise sit at "Would you like to log
// in to Amp? [(y)es, (n)o]:" inside its tmux pane until systemd's
// TimeoutStartSec killed the unit with a bare "start operation timed out"
// and no hint that auth was the problem (the same shape as the kilo
// missing-login incident recorded in session.kiloInstanceXDGEnvForHome).
//
// An unrecognized failure (network down, a broken install) is reported as
// "could not confirm" rather than "not logged in", so the operator is not
// sent chasing a login that isn't actually missing. Both cases still block
// provisioning: failing closed here is cheap (creation is a one-off,
// interactive operation) and much kinder than the hang it prevents.
func ampAuthProblemVia(cmd *exec.Cmd) string {
	out, err := cmd.CombinedOutput()
	if err == nil {
		return ""
	}
	if strings.Contains(strings.ToLower(string(out)), "api key") {
		return "amp does not appear to be logged in (`amp usage` reports a missing or invalid API key)"
	}
	return fmt.Sprintf("could not confirm amp's login state (`amp usage` failed: %v: %s)", err, firstLine(string(out), 200))
}

// firstLine reduces a command's output to a single, length-capped line so an
// error message stays readable in a systemd journal and can't smuggle
// newlines into a multi-line report.
func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}
