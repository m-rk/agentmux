package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/m-rk/agentmux/daemon/internal/taskcheck"
)

type taskcheckRequest struct {
	Instance string `json:"instance"`
}

func decodeTaskcheckRequest(reader io.Reader) (taskcheckRequest, error) {
	decoder := json.NewDecoder(io.LimitReader(reader, 1024))
	decoder.DisallowUnknownFields()
	var request taskcheckRequest
	if err := decoder.Decode(&request); err != nil {
		return taskcheckRequest{}, err
	}
	if request.Instance == "" {
		return taskcheckRequest{}, errors.New("missing task instance")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return taskcheckRequest{}, errors.New("broker request must contain exactly one JSON object")
	}
	return request, nil
}

// runTaskcheckBroker is the dedicated root service entry point. It accepts
// no task, command, path, or other caller-controlled arguments.
func runTaskcheckBroker(args []string) {
	if len(args) != 1 || args[0] != "serve" {
		fmt.Fprintln(os.Stderr, "usage: agentmux taskcheck-broker serve")
		os.Exit(2)
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "task-check broker service must run as root")
		os.Exit(1)
	}
	if err := serveTaskcheckBroker(); err != nil {
		fmt.Fprintf(os.Stderr, "task-check broker: %v\n", err)
		os.Exit(1)
	}
}

func printTaskcheckResult(result taskcheck.Result) {
	fmt.Print(result.Output)
	if result.OutputTruncated {
		fmt.Println("[output truncated at 3 MiB]")
	}
	fmt.Printf("AGENTMUX_TEST_RESULT=%s project=%s exit_code=%d duration_ms=%d\n",
		result.Category, result.Project, result.ExitCode, result.Duration.Milliseconds())
}
