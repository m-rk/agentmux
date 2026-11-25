package probe

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/catalogue"
)

// Every amp probe on a host continues one standing thread instead of
// starting a new one each hour: amp pushes a phone notice for each new
// thread, and one standing thread can be muted once.
const (
	// AmpThreadPrefix opens the standing thread's first message. The amp
	// sweep keeps any thread that starts with it (see ampsweep).
	AmpThreadPrefix = "[agentmux provider probe]"
	// AmpThreadTitle names the standing thread in the Amp sidebar.
	AmpThreadTitle = "agentmux provider probe"
	// AmpThreadLabel is the label the standing thread carries.
	AmpThreadLabel = "agentmux-probe"
	// AmpRollAge and AmpRollTurns bound the thread's context: past
	// either, the next probe starts a new thread. Each roll means a new
	// thread to mute, so both are far apart (about a month of hourly
	// probes).
	AmpRollAge   = 30 * 24 * time.Hour
	AmpRollTurns = 1000
)

// AmpThread is the standing thread's state, kept in AmpThreadPath.
type AmpThread struct {
	ID        string    `json:"id"`
	StartedAt time.Time `json:"started_at"`
	Turns     int       `json:"turns"`
}

// AmpThreadPath is the standing thread's state file under home.
func AmpThreadPath(home string) string {
	return filepath.Join(home, ".local", "state", "agentmux", "amp-probe-thread.json")
}

// due reports whether the thread should be replaced by a new one.
func (t AmpThread) due(now time.Time) bool {
	return t.Turns >= AmpRollTurns || (!t.StartedAt.IsZero() && now.Sub(t.StartedAt) >= AmpRollAge)
}

// LoadAmpThread reads the state file. A missing file is no thread.
func LoadAmpThread(path string) (AmpThread, error) {
	var t AmpThread
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return t, nil
	}
	if err != nil {
		return t, err
	}
	return t, json.Unmarshal(b, &t)
}

// SaveAmpThread writes the state file atomically.
func SaveAmpThread(path string, t AmpThread) error {
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".amp-probe-thread-*")
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

// AmpNewArgs starts the standing thread. --no-archive-after-execute keeps
// it continuable: amp archives a thread when an -x run ends, and an
// archived thread cannot be continued (see session.AmpRunArgs).
func AmpNewArgs(mode string) []string {
	return []string{"--stream-json", "-m", mode, "--title", AmpThreadTitle, "-l", AmpThreadLabel,
		"--no-archive-after-execute", "-x", AmpThreadPrefix + " " + Prompt}
}

// AmpContinueArgs asks the standing thread once more.
func AmpContinueArgs(mode, thread string) []string {
	return []string{"threads", "continue", thread, "--stream-json", "-m", mode,
		"--no-archive-after-execute", "-x", Prompt}
}

// threadGone reports whether amp refused a continue because the thread
// no longer exists or is archived.
func threadGone(text string) bool {
	low := strings.ToLower(text)
	return strings.Contains(low, "does not exist") || strings.Contains(low, "is archived")
}

// AmpStanding sends amp tries to the host's standing thread.
type AmpStanding struct {
	// Path is the state file (AmpThreadPath).
	Path string
	Now  func() time.Time
	// Log gets one line per thread event (continued, started, rolled,
	// gone), for the probe log.
	Log func(string)
}

func (s *AmpStanding) logf(format string, a ...any) {
	if s.Log != nil {
		s.Log(fmt.Sprintf(format, a...))
	}
}

// try continues the standing thread, or starts one when there is none,
// it is due to roll, or amp says it is gone. run does one amp process.
func (s *AmpStanding) try(t Target, run func(args []string) Attempt) Attempt {
	now := s.Now()
	cur, err := LoadAmpThread(s.Path)
	if err != nil {
		s.logf("amp probe thread: unreadable state (%v); starting a new thread", err)
		cur = AmpThread{}
	}
	if cur.ID != "" && cur.due(now) {
		s.logf("amp probe thread %s: %d turns since %s; rolling to a new thread", cur.ID, cur.Turns, cur.StartedAt.UTC().Format(time.RFC3339))
		cur = AmpThread{}
	}
	if cur.ID != "" {
		a := run(AmpContinueArgs(t.Model, cur.ID))
		if !a.threadGone {
			cur.Turns++
			s.save(cur)
			s.logf("amp probe thread %s: continued (turn %d)", cur.ID, cur.Turns)
			return a
		}
		s.logf("amp probe thread %s: gone or archived; starting a new thread", cur.ID)
	}
	a := run(AmpNewArgs(t.Model))
	if a.Session == "" {
		s.logf("amp probe thread: amp started no thread")
		return a
	}
	cur = AmpThread{ID: a.Session, StartedAt: now, Turns: 1}
	s.save(cur)
	s.logf("amp probe thread %s: started", cur.ID)
	return a
}

func (s *AmpStanding) save(t AmpThread) {
	if err := SaveAmpThread(s.Path, t); err != nil {
		s.logf("amp probe thread %s: saving state: %v", t.ID, err)
	}
}

// isAmp reports whether t goes to the standing thread.
func (s *AmpStanding) isAmp(t Target) bool {
	return s != nil && t.Runner == catalogue.RunnerAmp
}
