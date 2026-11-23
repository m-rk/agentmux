package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Stopping a headless run (`sessions stop`, docs/codex-run.md and
// docs/amp-run.md). The stop appends an agentmux.stop record to the run's
// stream log, then ends the run's processes: everything below the launcher
// gets SIGTERM, and whatever is left after a grace period SIGKILL. The
// launcher itself is spared, so it still writes its agentmux.exit record:
// that record is what tells a reader the run is over, not busy. A stop
// record followed by the exit record reads as state "stopped". The instance,
// its worktree and its branch are untouched, and the thread can be
// continued like any finished one.

// runStopRecord is the agentmux.stop line.
type runStopRecord struct {
	Type string `json:"type"`
	Run  string `json:"run,omitempty"`
	By   string `json:"by,omitempty"`
	At   string `json:"at"`
}

// runStopGrace is how long the run gets after SIGTERM, and then again after
// SIGKILL, to be gone and have its exit record written; replaceable in tests.
var runStopGrace = 10 * time.Second

// appendRunRecord appends one JSON line to a run log. The launcher holds the
// log open with O_APPEND too, and a single short write never interleaves
// with its lines.
func appendRunRecord(logPath string, rec any) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// StopCodexRun stops the running turn whose stream log is logPath, as by.
// It returns the state afterwards and whether anything was stopped; a log
// whose run is not running (done, failed, already stopped) is left alone.
// The launcher's own exit record ends the run; if the launcher is already
// gone, the stop writes that record itself.
func StopCodexRun(ctx context.Context, instance, workdir, logPath, by string) (CodexRunState, bool, error) {
	st := CodexRunStateOf(logPath)
	if st.State != "running" || !fileIsRegular(logPath) {
		return st, false, nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if err := appendRunRecord(logPath, runStopRecord{Type: "agentmux.stop", Run: st.run, By: by, At: now}); err != nil {
		return st, false, fmt.Errorf("recording the stop: %w", err)
	}
	pids := func() []int {
		return unionPids(descendantPids(st.pid), codexRunPidsForInstance(instance, workdir))
	}
	over := func() bool { return CodexRunStateOf(logPath).State != "running" }
	if !endRun(ctx, pids, over) {
		if st.pid > 0 && codexRunPIDAlive(st.pid) {
			return CodexRunStateOf(logPath), true, fmt.Errorf("codex run did not exit within %s of SIGKILL", 2*runStopGrace)
		}
		// No launcher left to write the exit record: write it, so the run
		// never reads as busy.
		_ = appendRunRecord(logPath, map[string]any{"type": "agentmux.exit", "run": st.run, "code": -1, "at": time.Now().UTC().Format(time.RFC3339)})
	}
	return CodexRunStateOf(logPath), true, nil
}

// StopAmpRunThread is StopCodexRun for an amp run thread: the stop record,
// then every amp run child of the instance in workdir (see StopAmpRuns),
// which the amp launcher outlives to write its exit record.
func StopAmpRunThread(ctx context.Context, instance, workdir, logPath, by string) (AmpRunState, bool, error) {
	st := AmpRunStateOf(logPath)
	if (st.State != "running" && st.State != "waiting") || !fileIsRegular(logPath) {
		return st, false, nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if err := appendRunRecord(logPath, runStopRecord{Type: "agentmux.stop", By: by, At: now}); err != nil {
		return st, false, fmt.Errorf("recording the stop: %w", err)
	}
	pids := func() []int { return ampRunPidsForInstance(instance, workdir) }
	over := func() bool { s := AmpRunStateOf(logPath).State; return s != "running" && s != "waiting" }
	if !endRun(ctx, pids, over) {
		if len(pids()) > 0 {
			return AmpRunStateOf(logPath), true, fmt.Errorf("amp run did not exit within %s of SIGKILL", 2*runStopGrace)
		}
		// The amp launcher records no pid, so with no amp process left a
		// missing exit record means the launcher is gone too.
		_ = appendRunRecord(logPath, map[string]any{"type": "agentmux.exit", "code": -1, "at": time.Now().UTC().Format(time.RFC3339)})
	}
	return AmpRunStateOf(logPath), true, nil
}

// endRun sends SIGTERM to pids(), waits up to runStopGrace for over(), then
// SIGKILLs what is left and waits once more. It reports whether over() came
// true.
func endRun(ctx context.Context, pids func() []int, over func() bool) bool {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		for _, pid := range pids() {
			_ = syscall.Kill(pid, sig)
		}
		deadline := time.Now().Add(runStopGrace)
		for {
			if over() {
				return true
			}
			if time.Now().After(deadline) {
				break
			}
			select {
			case <-ctx.Done():
				return over()
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	return over()
}

func fileIsRegular(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// descendantPids lists every process below root (not root itself), from
// /proc. Linux only; elsewhere, and for root <= 0, it is empty.
func descendantPids(root int) []int {
	if root <= 0 {
		return nil
	}
	stats, _ := filepath.Glob("/proc/[0-9]*/stat")
	children := map[int][]int{}
	for _, p := range stats {
		pid, ppid, ok := procParent(p)
		if ok {
			children[ppid] = append(children[ppid], pid)
		}
	}
	var out []int
	queue := []int{root}
	seen := map[int]bool{root: true}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		for _, c := range children[next] {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
				queue = append(queue, c)
			}
		}
	}
	return out
}

// procParent reads pid and ppid from a /proc/<pid>/stat file. The command
// name is in parentheses and may hold spaces, so fields are counted from the
// last ')'.
func procParent(statPath string) (pid, ppid int, ok bool) {
	data, err := os.ReadFile(statPath)
	if err != nil {
		return 0, 0, false
	}
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, 0, false
	}
	fields := strings.Fields(s[i+1:])
	if len(fields) < 2 {
		return 0, 0, false
	}
	pid, err1 := strconv.Atoi(strings.TrimSpace(s[:strings.IndexByte(s, ' ')]))
	ppid, err2 := strconv.Atoi(fields[1])
	return pid, ppid, err1 == nil && err2 == nil
}

func unionPids(a, b []int) []int {
	seen := map[int]bool{}
	var out []int
	for _, list := range [][]int{a, b} {
		for _, p := range list {
			if p > 0 && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}
