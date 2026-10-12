package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/liveguard"
	"github.com/m-rk/agentmux/daemon/internal/taskcheck"
)

// runSessionsCheck executes the project-approved full test command on the
// daemon host, outside the Codex command sandbox. It accepts no target path,
// project or command: the task instance is stamped into the worker env, and
// the root broker resolves all execution details from the registry.
func runSessionsCheck(args []string) {
	fs := flag.NewFlagSet("sessions check", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print machine-readable output")
	fs.Parse(args)
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: agentmux sessions check [-json]")
		os.Exit(2)
	}
	instance := os.Getenv(liveguard.InstanceEnv)
	if os.Getenv(liveguard.TaskEnv) != "1" || !strings.HasPrefix(instance, liveguard.TaskPrefix) {
		fmt.Fprintln(os.Stderr, "agentmux sessions check is available only inside an agentmux task session")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 31*time.Minute)
	defer cancel()
	result, err := callTaskcheckBroker(ctx, instance)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentmux sessions check: broker unavailable: %v\n", err)
		os.Exit(1)
	}
	if *jsonOut {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"result": result.Category, "project": result.Project, "output": result.Output,
			"exit_code": result.ExitCode, "duration_ms": result.Duration.Milliseconds(),
			"output_truncated": result.OutputTruncated,
		})
	} else {
		printTaskcheckResult(result)
	}
	if result.ExitCode != 0 {
		if result.ExitCode > 0 && result.ExitCode < 126 {
			os.Exit(result.ExitCode)
		}
		os.Exit(1)
	}
}

const (
	taskcheckBrokerRoot       = "/tmp/agentmux-taskcheck-broker"
	taskcheckBrokerRequests   = taskcheckBrokerRoot + "/requests"
	taskcheckBrokerResponses  = taskcheckBrokerRoot + "/responses"
	taskcheckBrokerProcessing = taskcheckBrokerRoot + "/processing"
)

func callTaskcheckBroker(ctx context.Context, instance string) (taskcheck.Result, error) {
	if err := validateTaskcheckBrokerDirs(taskcheckBrokerRoot, 0); err != nil {
		return taskcheck.Result{}, fmt.Errorf("task-check broker setup: %w", err)
	}
	return callTaskcheckBrokerAt(ctx, instance, os.Getuid(), taskcheckBrokerRequests, taskcheckBrokerResponses)
}

func validateTaskcheckBrokerDirs(root string, owner uint32) error {
	for _, dir := range []struct {
		path string
		mode os.FileMode
	}{
		{root, 0o755},
		{filepath.Join(root, "requests"), os.ModeSticky | 0o733},
		{filepath.Join(root, "responses"), 0o711},
		{filepath.Join(root, "processing"), 0o700},
	} {
		info, err := os.Lstat(dir.path)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != owner || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("broker directory %q has unsafe ownership or type", dir.path)
		}
		if info.Mode().Perm() != dir.mode.Perm() || info.Mode()&os.ModeSticky != dir.mode&os.ModeSticky {
			return fmt.Errorf("broker directory %q has unsafe permissions", dir.path)
		}
	}
	return nil
}

func callTaskcheckBrokerAt(ctx context.Context, instance string, uid int, requestsDir, responsesDir string) (taskcheck.Result, error) {
	var requestID [16]byte
	if _, err := rand.Read(requestID[:]); err != nil {
		return taskcheck.Result{}, fmt.Errorf("creating broker request ID: %w", err)
	}
	id := hex.EncodeToString(requestID[:])
	name := strconv.Itoa(uid) + "-" + id + ".json"
	requestPath := filepath.Join(requestsDir, name)
	body, err := json.Marshal(struct {
		Instance string `json:"instance"`
	}{Instance: instance})
	if err != nil {
		return taskcheck.Result{}, err
	}
	f, err := os.OpenFile(requestPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return taskcheck.Result{}, fmt.Errorf("submitting task-check request: %w", err)
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		_ = os.Remove(requestPath)
		return taskcheck.Result{}, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(requestPath)
		return taskcheck.Result{}, err
	}
	responsePath := filepath.Join(responsesDir, name)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if result, ok, err := readTaskcheckResponse(responsePath, uid); err != nil {
			return taskcheck.Result{}, err
		} else if ok {
			return result, nil
		}
		select {
		case <-ctx.Done():
			_ = os.Remove(requestPath)
			return taskcheck.Result{}, fmt.Errorf("waiting for task-check broker: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func readTaskcheckResponse(path string, uid int) (taskcheck.Result, bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return taskcheck.Result{}, false, nil
	}
	if err != nil {
		return taskcheck.Result{}, false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != uid || !info.Mode().IsRegular() || info.Mode().Perm() != 0o400 {
		return taskcheck.Result{}, false, fmt.Errorf("task-check response failed ownership checks")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return taskcheck.Result{}, false, err
	}
	var result taskcheck.Result
	if err := json.Unmarshal(data, &result); err != nil {
		return taskcheck.Result{}, false, err
	}
	return result, true, nil
}
