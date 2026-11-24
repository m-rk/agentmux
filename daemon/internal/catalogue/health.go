package catalogue

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// Provider health is what `agentmux models probe` found when it last sent
// each provider tank one tiny request. `models list -json` carries it under
// "providers", next to "catalogue". Like Offering, the JSON shape is a
// contract for other programs (UP-231): add fields, don't rename them.

// Health statuses.
const (
	HealthOK      = "ok"
	HealthDown    = "down"
	HealthUnknown = "unknown" // never probed, or the host could not say
)

// Error kinds: the class of the last failure, in a word.
const (
	ErrKindAuth    = "auth"
	ErrKindQuota   = "quota"
	ErrKindNetwork = "network"
	ErrKindTimeout = "timeout"
	ErrKindOther   = "other"
)

// ProviderHealth is the last probe of one provider tank on one host.
type ProviderHealth struct {
	// Provider is the tank: claude, chatgpt, amp, or an opencode/kilo
	// provider such as ollama.
	Provider string `json:"provider"`
	Label    string `json:"label"`
	// Runner is the runner the probe went through; Model the model or,
	// for amp, the mode it asked.
	Runner string `json:"runner"`
	Model  string `json:"model"`
	// Host and HostLabel match the catalogue's hosts[].host and .label.
	Host      string `json:"host"`
	HostLabel string `json:"host_label"`
	Status    string `json:"status"`
	// CheckedAt is when the last probe finished; LastOKAt the last time
	// one passed; DownSince the first probe of the current down run.
	// RFC 3339, or empty.
	CheckedAt string `json:"checked_at"`
	LastOKAt  string `json:"last_ok_at"`
	DownSince string `json:"down_since"`
	// ErrorKind and Error describe the last probe's failure, empty when
	// it passed. Error is fixed plain words: never runner output, paths,
	// hostnames or tokens.
	ErrorKind string `json:"error_kind"`
	Error     string `json:"error"`
	// LatencyMS is the last try's time to answer; Attempts the tries the
	// last probe took (1-3).
	LatencyMS int64 `json:"latency_ms"`
	Attempts  int   `json:"attempts"`
}

// Key is the state key of a row: provider and host.
func (h ProviderHealth) Key() string { return h.Provider + "@" + h.Host }

// HealthState is the probe state file.
type HealthState struct {
	Version   int              `json:"v"`
	Providers []ProviderHealth `json:"providers"`
}

// HealthPath is the probe state file under home.
func HealthPath(home string) string {
	return filepath.Join(home, ".local", "state", "agentmux", "provider-health.json")
}

// LoadHealth reads the state file. A missing file is no rows.
func LoadHealth(path string) (HealthState, error) {
	var s HealthState
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(b, &s)
	return s, err
}

// SaveHealth writes the state file atomically, rows sorted by key.
func SaveHealth(path string, s HealthState) error {
	s.Version = 1
	sort.Slice(s.Providers, func(i, j int) bool { return s.Providers[i].Key() < s.Providers[j].Key() })
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".provider-health-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
