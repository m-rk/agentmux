// Package modelsource loads the per-host registry and checks source health.
package modelsource

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Source struct {
	Name          string   `yaml:"name" json:"name"`
	Kind          string   `yaml:"kind" json:"kind"`
	Endpoint      string   `yaml:"endpoint" json:"endpoint"`
	CredentialRef string   `yaml:"credential_ref,omitempty" json:"-"`
	Cost          string   `yaml:"cost" json:"cost"`
	Limits        []string `yaml:"limits,omitempty" json:"limits,omitempty"`
	Expires       string   `yaml:"expires,omitempty" json:"expires,omitempty"`
	Telemetry     bool     `yaml:"telemetry" json:"telemetry"`
	Training      bool     `yaml:"training" json:"training"`
}

type Registry struct {
	Sources []Source `yaml:"sources"`
}
type Result struct {
	Source    Source `json:"source"`
	Reachable bool   `json:"reachable"`
	Reason    string `json:"reason,omitempty"`
}

func DefaultPath() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(h, ".config", "agentmux", "models.yaml")
}

func Load(path string) (Registry, error) {
	var r Registry
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	if err := yaml.Unmarshal(b, &r); err != nil {
		return r, err
	}
	seen := map[string]bool{}
	for i, s := range r.Sources {
		if strings.TrimSpace(s.Name) == "" || seen[s.Name] {
			return r, fmt.Errorf("source %d has an empty or duplicate name", i+1)
		}
		seen[s.Name] = true
		switch s.Kind {
		case "ollama-cloud", "ollama-local", "openai-compatible", "subscription":
		default:
			return r, fmt.Errorf("source %q has unsupported kind %q", s.Name, s.Kind)
		}
		if s.Kind != "subscription" && !validEndpoint(s.Endpoint) {
			return r, fmt.Errorf("source %q requires an http(s) endpoint", s.Name)
		}
		switch s.Cost {
		case "free", "subscription", "metered":
		default:
			return r, fmt.Errorf("source %q has unsupported cost %q", s.Name, s.Cost)
		}
		if s.CredentialRef != "" && !strings.HasPrefix(s.CredentialRef, "op://") {
			return r, fmt.Errorf("source %q credential_ref must be an op:// reference", s.Name)
		}
	}
	return r, nil
}

func validEndpoint(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func Check(ctx context.Context, sources []Source, executable string, now time.Time) []Result {
	out := make([]Result, 0, len(sources))
	for _, s := range sources {
		r := Result{Source: s}
		if s.Expires != "" {
			d, e := time.Parse("2006-01-02", s.Expires)
			if e != nil {
				r.Reason = "invalid expiry date"
				out = append(out, r)
				continue
			}
			if now.After(d.Add(24*time.Hour - time.Nanosecond)) {
				r.Reason = "expired"
				out = append(out, r)
				continue
			}
		}
		if s.Kind == "subscription" && s.Endpoint == "" {
			r.Reason = "subscription source has no endpoint to check"
			out = append(out, r)
			continue
		}
		args := []string{"__models-probe", s.Kind, s.Endpoint}
		var cmd *exec.Cmd
		if s.CredentialRef != "" {
			envFile, e := os.CreateTemp("", "agentmux-models-*.env")
			if e != nil {
				r.Reason = "cannot prepare credential lookup"
				out = append(out, r)
				continue
			}
			_, _ = fmt.Fprintf(envFile, "MODEL_SOURCE_TOKEN=%s\n", s.CredentialRef)
			_ = envFile.Close()
			defer os.Remove(envFile.Name())
			cmdArgs := append([]string{"run", "--env-file", envFile.Name(), "--", executable}, args...)
			cmd = exec.CommandContext(ctx, "op", cmdArgs...)
		} else {
			cmd = exec.CommandContext(ctx, executable, args...)
		}
		if e := cmd.Run(); e != nil {
			r.Reason = "unreachable"
		} else {
			r.Reachable = true
		}
		out = append(out, r)
	}
	return out
}

// ProbeHelper is invoked only as a child process (under op run when needed).
func ProbeHelper(args []string) int {
	if len(args) != 2 || !validEndpoint(args[1]) {
		return 2
	}
	kind, endpoint := args[0], strings.TrimRight(args[1], "/")
	path := "/models"
	if strings.HasPrefix(kind, "ollama-") {
		path = "/api/tags"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+path, nil)
	if token := os.Getenv("MODEL_SOURCE_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 1
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]bool{"reachable": true})
	return 0
}
