package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/taskcheck"
)

func TestTaskcheckClientUsesOnlyFixedFileRequest(t *testing.T) {
	root := t.TempDir()
	requestsDir, responsesDir := filepath.Join(root, "requests"), filepath.Join(root, "responses")
	if err := os.Mkdir(requestsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(responsesDir, 0o700); err != nil {
		t.Fatal(err)
	}
	brokerDone := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			entries, err := os.ReadDir(requestsDir)
			if err != nil {
				brokerDone <- err
				return
			}
			if len(entries) > 0 {
				name := entries[0].Name()
				requestBytes, err := os.ReadFile(filepath.Join(requestsDir, name))
				if err != nil {
					brokerDone <- err
					return
				}
				request, err := decodeTaskcheckRequest(strings.NewReader(string(requestBytes)))
				if err != nil || request.Instance != "task-76" {
					if err == nil {
						err = fmt.Errorf("fake broker received unexpected instance %q", request.Instance)
					}
					brokerDone <- err
					return
				}
				response, _ := json.Marshal(taskcheck.Result{Project: "m-rk/agentmux", Category: "passed", ExitCode: 0})
				responsePath := filepath.Join(responsesDir, name)
				if err := os.WriteFile(responsePath, response, 0o400); err != nil {
					brokerDone <- err
					return
				}
				brokerDone <- nil
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		brokerDone <- context.DeadlineExceeded
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := callTaskcheckBrokerAt(ctx, "task-76", os.Getuid(), requestsDir, responsesDir)
	if err != nil || got.Category != "passed" {
		t.Fatalf("broker result = %+v, %v", got, err)
	}
	if err := <-brokerDone; err != nil {
		t.Fatalf("fake broker: %v", err)
	}
}

func TestTaskcheckClientReportsMailboxSetupDenial(t *testing.T) {
	_, err := callTaskcheckBrokerAt(context.Background(), "task-76", os.Getuid(), filepath.Join(t.TempDir(), "missing"), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "submitting task-check request") {
		t.Fatalf("mailbox setup denial = %v", err)
	}
}

func TestTaskcheckResponseRequiresRequestingUserOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "response.json")
	if err := os.WriteFile(path, []byte(`{"Category":"passed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := readTaskcheckResponse(path, os.Getuid()); err == nil || ok {
		t.Fatalf("accepted response with unsafe mode: ok=%v err=%v", ok, err)
	}
}

func TestTaskcheckBrokerDirectoryPermissions(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []struct {
		name string
		mode os.FileMode
	}{
		{"requests", os.ModeSticky | 0o733},
		{"responses", 0o711},
		{"processing", 0o700},
	} {
		path := filepath.Join(root, dir.name)
		if err := os.Mkdir(path, dir.mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, dir.mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := validateTaskcheckBrokerDirs(root, uint32(os.Getuid())); err != nil {
		t.Fatalf("valid broker dirs refused: %v", err)
	}
	if err := os.Chmod(filepath.Join(root, "responses"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := validateTaskcheckBrokerDirs(root, uint32(os.Getuid())); err == nil {
		t.Fatal("accepted world-writable response directory")
	}
}

func TestTaskcheckBrokerRejectsArbitraryCommandsAndPaths(t *testing.T) {
	for _, body := range []string{
		`{"instance":"task-76","command":"cat /etc/passwd"}`,
		`{"instance":"task-76","path":"/etc/passwd"}`,
		`{}`,
	} {
		if req, err := decodeTaskcheckRequest(strings.NewReader(body)); err == nil {
			t.Errorf("accepted untrusted broker request %+v from %s", req, body)
		}
	}
}
