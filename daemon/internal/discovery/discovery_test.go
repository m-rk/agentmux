package discovery

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParsePaneLine(t *testing.T) {
	cases := []struct {
		name        string
		line        string
		wantSession string
		wantPID     int64
		wantStarted int64
		wantOK      bool
	}{
		{
			name:        "plain session name",
			line:        "12345|1700000000|manualtest",
			wantSession: "manualtest",
			wantPID:     12345,
			wantStarted: 1700000000,
			wantOK:      true,
		},
		{
			name: "session name containing the delimiter itself",
			// tmux allows "|" in a session name; since session_name is the
			// last field and this uses SplitN(..., 3), a literal "|" here
			// must stay inside that final field rather than getting treated
			// as a fourth separator and shifting/truncating pid or
			// session_created.
			line:        "999|1700000001|weird|name|with|pipes",
			wantSession: "weird|name|with|pipes",
			wantPID:     999,
			wantStarted: 1700000001,
			wantOK:      true,
		},
		{
			name:   "too few fields is dropped, not partially parsed",
			line:   "12345|1700000000",
			wantOK: false,
		},
		{
			name:   "empty line is dropped",
			line:   "",
			wantOK: false,
		},
		{
			name:        "non-numeric pid/created still yields the session name",
			line:        "notanumber|alsonotanumber|somesession",
			wantSession: "somesession",
			wantPID:     0,
			wantStarted: 0,
			wantOK:      true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			session, pid, startedAt, ok := parsePaneLine(tc.line)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if session != tc.wantSession {
				t.Errorf("session = %q, want %q", session, tc.wantSession)
			}
			if pid != tc.wantPID {
				t.Errorf("pid = %d, want %d", pid, tc.wantPID)
			}
			wantStarted := time.Time{}
			if tc.wantStarted != 0 {
				wantStarted = time.Unix(tc.wantStarted, 0)
			}
			if !startedAt.Equal(wantStarted) {
				t.Errorf("startedAt = %v, want %v", startedAt, wantStarted)
			}
		})
	}
}

func TestParseEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.env")
	content := "" +
		"AGENTMUX_INSTANCE_NAME=probe\n" +
		"# a comment line, and a blank line follow\n" +
		"\n" +
		"AGENTMUX_WORKDIR=/home/testuser/.agentmux/probe\n" +
		"AGENTMUX_RESUME=\n" +
		"malformed line with no equals sign\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	fields, err := parseEnvFile(path)
	if err != nil {
		t.Fatalf("parseEnvFile: %v", err)
	}

	want := map[string]string{
		"AGENTMUX_INSTANCE_NAME": "probe",
		"AGENTMUX_WORKDIR":       "/home/testuser/.agentmux/probe",
		"AGENTMUX_RESUME":        "",
	}
	if len(fields) != len(want) {
		t.Fatalf("fields = %v, want %v", fields, want)
	}
	for k, v := range want {
		if fields[k] != v {
			t.Errorf("fields[%q] = %q, want %q", k, fields[k], v)
		}
	}
}

func TestParseEnvFileMissing(t *testing.T) {
	_, err := parseEnvFile(filepath.Join(t.TempDir(), "nope.env"))
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestInstanceFromEnv(t *testing.T) {
	now := time.Now()

	t.Run("dead when no matching pane", func(t *testing.T) {
		inst := instanceFromEnv("probe.env", map[string]string{
			"AGENTMUX_WORKDIR": "/w",
		}, nil)
		if inst.Name != "probe" {
			t.Errorf("Name = %q, want %q (derived from filename)", inst.Name, "probe")
		}
		if inst.Status != StatusDead {
			t.Errorf("Status = %v, want StatusDead", inst.Status)
		}
	})

	t.Run("running when pane activity is recent", func(t *testing.T) {
		panes := map[string]tmuxPane{
			"probe": {pid: 42, lastChanged: now, socket: "/tmp/tmux-1000/agentmux-probe", startedAt: now},
		}
		inst := instanceFromEnv("probe.env", map[string]string{
			"AGENTMUX_INSTANCE_NAME": "probe",
		}, panes)
		if inst.Status != StatusRunning {
			t.Errorf("Status = %v, want StatusRunning", inst.Status)
		}
		if inst.PID != 42 {
			t.Errorf("PID = %d, want 42", inst.PID)
		}
	})

	t.Run("idle when pane activity is stale", func(t *testing.T) {
		stale := now.Add(-idleThreshold - time.Second)
		panes := map[string]tmuxPane{
			"probe": {pid: 42, lastChanged: stale, socket: "/tmp/tmux-1000/agentmux-probe", startedAt: stale},
		}
		inst := instanceFromEnv("probe.env", map[string]string{
			"AGENTMUX_INSTANCE_NAME": "probe",
		}, panes)
		if inst.Status != StatusIdle {
			t.Errorf("Status = %v, want StatusIdle", inst.Status)
		}
	})

	t.Run("session name prefers AGENTMUX_TMUX_SESSION_NAME over AGENTMUX_SESSION_NAME over name", func(t *testing.T) {
		inst := instanceFromEnv("probe.env", map[string]string{
			"AGENTMUX_INSTANCE_NAME":     "probe",
			"AGENTMUX_TMUX_SESSION_NAME": "tmux-name",
			"AGENTMUX_SESSION_NAME":      "session-name",
		}, nil)
		if inst.TmuxSession != "tmux-name" {
			t.Errorf("TmuxSession = %q, want %q", inst.TmuxSession, "tmux-name")
		}
	})

	t.Run("agent defaults to claude-code when unset (bash claude-code registry never sets it)", func(t *testing.T) {
		inst := instanceFromEnv("probe.env", map[string]string{
			"AGENTMUX_INSTANCE_NAME": "probe",
		}, nil)
		if inst.Agent != "claude-code" {
			t.Errorf("Agent = %q, want %q", inst.Agent, "claude-code")
		}
	})

	t.Run("agent uses AGENTMUX_AGENT when set", func(t *testing.T) {
		inst := instanceFromEnv("probe.env", map[string]string{
			"AGENTMUX_INSTANCE_NAME": "probe",
			"AGENTMUX_AGENT":         "zero",
		}, nil)
		if inst.Agent != "zero" {
			t.Errorf("Agent = %q, want %q", inst.Agent, "zero")
		}
	})
}
