package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/daemoninstall"
	"github.com/m-rk/agentmux/daemon/internal/liveguard"
	"github.com/m-rk/agentmux/daemon/internal/ops"
)

// runSessionsCheck executes the project-approved full test command on the
// daemon host, outside the Codex command sandbox. It accepts no target path,
// project or command: the task instance is stamped into the worker env, and
// the daemon resolves all execution details from its registry.
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
	result, err := (ops.Env{SocketPath: daemoninstall.SocketPath()}).RunTaskCheck(ctx, instance)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentmux sessions check: %v\n", err)
		os.Exit(1)
	}
	if *jsonOut {
		writeJSON(result)
	} else {
		fmt.Print(result.Output)
		if result.OutputTruncated {
			fmt.Fprintln(os.Stdout, "[output truncated at 3 MiB]")
		}
		fmt.Printf("AGENTMUX_TEST_RESULT=%s project=%s exit_code=%d duration_ms=%d\n", result.Result, result.Project, result.ExitCode, result.DurationMs)
	}
	if result.ExitCode != 0 {
		if result.ExitCode > 0 && result.ExitCode < 126 {
			os.Exit(int(result.ExitCode))
		}
		os.Exit(1)
	}
}
