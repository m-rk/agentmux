package collab

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// State records what one managed session has already seen. It deliberately
// lives outside the instance registry: cursors are runtime state, not
// provisioning configuration.
type State struct {
	// Onboarded marks whether this instance has ever been sent the
	// collaboration-commands paragraph. Deliberately not tied to the tmux
	// session's identity: a restart (nightly compact-and-restart included)
	// changes that identity but doesn't make an established instance new
	// again, so onboarding fires once per instance's lifetime, not once
	// per restart.
	Onboarded bool              `json:"onboarded,omitempty"`
	LastSeen  map[string]string `json:"last_seen,omitempty"`
	UpdatedAt time.Time         `json:"updated_at,omitempty"`
}

func StatePath(home, instance string) string {
	safe := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r) {
			return r
		}
		return '_'
	}, instance)
	return filepath.Join(home, ".local", "state", "agentmux", "collab", safe+".json")
}

func LoadState(path string) (State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return State{LastSeen: map[string]string{}}, nil
		}
		return State{}, fmt.Errorf("reading collaboration state: %w", err)
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("parsing collaboration state: %w", err)
	}
	if state.LastSeen == nil {
		state.LastSeen = map[string]string{}
	}
	return state, nil
}

func SaveState(path string, state State) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating collaboration state directory: %w", err)
	}
	state.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding collaboration state: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		return fmt.Errorf("creating collaboration state: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing collaboration state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing collaboration state: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("saving collaboration state: %w", err)
	}
	return nil
}
