package provision

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// EnvKeyPrefix prefixes the registry keys holding an instance's launch
// environment (one line per variable, e.g. AGENTMUX_ENV_OTEL_SERVICE_NAME).
// The values ride every launch of the agent, including the nightly restart.
const EnvKeyPrefix = "AGENTMUX_ENV_"

var otelKeyRE = regexp.MustCompile(`^OTEL_[A-Z0-9_]+$`)

// telemetryKeyAllowed reports whether key may be set with -env: OTEL_*
// plus the one Claude Code telemetry switch.
func telemetryKeyAllowed(key string) bool {
	return key == "CLAUDE_CODE_ENABLE_TELEMETRY" || otelKeyRE.MatchString(key)
}

// CleanTelemetryEnv validates KEY=VALUE pairs and returns them as registry
// lines. Errors name the key only, never the value (an OTLP header can
// carry a token).
func CleanTelemetryEnv(pairs []string) ([]kv, error) {
	seen := map[string]bool{}
	var out []kv
	for i, p := range pairs {
		key, val, ok := strings.Cut(p, "=")
		if key == "" {
			return nil, fmt.Errorf("-env entry %d has no key (want KEY=VALUE)", i+1)
		}
		if !telemetryKeyAllowed(key) {
			return nil, fmt.Errorf("-env key %q is not allowed: only OTEL_* and CLAUDE_CODE_ENABLE_TELEMETRY", key)
		}
		if !ok {
			return nil, fmt.Errorf("-env %s has no value (want KEY=VALUE)", key)
		}
		if strings.ContainsAny(val, "\r\n\x00") {
			return nil, fmt.Errorf("-env %s: value contains a line break or NUL byte", key)
		}
		if seen[key] {
			return nil, fmt.Errorf("-env %s given more than once", key)
		}
		seen[key] = true
		out = append(out, kv{EnvKeyPrefix + key, val})
	}
	return out, nil
}

// ValidateTelemetryEnv checks pairs without storing them.
func ValidateTelemetryEnv(pairs []string) error {
	_, err := CleanTelemetryEnv(pairs)
	return err
}

// EnvKeys returns the variable names in pairs (no values), for messages.
func EnvKeys(pairs []string) []string {
	keys := make([]string, 0, len(pairs))
	for _, p := range pairs {
		k, _, _ := strings.Cut(p, "=")
		keys = append(keys, k)
	}
	return keys
}

// EnvFromRegistry returns the instance's stored launch environment as
// sorted KEY=VALUE pairs.
func EnvFromRegistry(fields map[string]string) []string {
	var out []string
	for k, v := range fields {
		if name, ok := strings.CutPrefix(k, EnvKeyPrefix); ok && name != "" {
			out = append(out, name+"="+v)
		}
	}
	sort.Strings(out)
	return out
}

// RegistryEnvMatches reports whether the stored env equals pairs (order
// ignored), for refusing a different env on an existing instance.
func RegistryEnvMatches(fields map[string]string, pairs []string) bool {
	have := EnvFromRegistry(fields)
	want := append([]string(nil), pairs...)
	sort.Strings(want)
	if len(have) != len(want) {
		return false
	}
	for i := range have {
		if have[i] != want[i] {
			return false
		}
	}
	return true
}

// telemetryEnvFor validates opts.Env for the agent: allowed for claude-code
// and codex only.
func telemetryEnvFor(opts Options) ([]kv, error) {
	if len(opts.Env) == 0 {
		return nil, nil
	}
	if opts.Agent != "claude-code" && opts.Agent != "codex" {
		return nil, fmt.Errorf("-env is supported for claude-code and codex only, not %q", opts.Agent)
	}
	return CleanTelemetryEnv(opts.Env)
}
