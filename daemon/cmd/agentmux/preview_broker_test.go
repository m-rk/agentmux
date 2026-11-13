package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/preview"
)

func TestDecodePreviewRequest(t *testing.T) {
	req, err := decodePreviewRequest(strings.NewReader(`{"instance":"task-a","path":"/w/dist"}`))
	if err != nil || req.Page != "/" || req.Width != preview.DefaultWidth {
		t.Fatalf("good: %+v, %v", req, err)
	}
	for name, body := range map[string]string{
		"unknown field": `{"instance":"task-a","path":"/w/dist","command":"rm -rf /"}`,
		"two objects":   `{"instance":"task-a","path":"/w"}{"instance":"task-b","path":"/w"}`,
		"not json":      `hello`,
		"no path":       `{"instance":"task-a"}`,
		"huge":          `{"instance":"task-a","path":"` + strings.Repeat("a", 5000) + `"}`,
	} {
		if _, err := decodePreviewRequest(strings.NewReader(body)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

// The worker side writes one request file and waits for the broker's answer.
func TestCallPreviewBrokerAt(t *testing.T) {
	requests, responses := t.TempDir(), t.TempDir()
	uid := os.Getuid()
	go func() {
		for {
			entries, _ := os.ReadDir(requests)
			for _, e := range entries {
				data, err := os.ReadFile(filepath.Join(requests, e.Name()))
				if err != nil {
					continue
				}
				var req preview.Request
				_ = json.Unmarshal(data, &req)
				out, _ := json.Marshal(preview.Result{OK: true, Path: "/store/" + req.Instance + ".png"})
				p := filepath.Join(responses, e.Name())
				_ = os.WriteFile(p, out, 0o400)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := callPreviewBrokerAt(ctx, preview.Request{Instance: "task-a", Path: "/w/dist"}, uid, requests, responses)
	if err != nil || !res.OK || res.Path != "/store/task-a.png" {
		t.Fatalf("got %+v, %v", res, err)
	}
	info, err := os.Stat(filepath.Join(requests, mustOnly(t, requests)))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("request file mode: %v %v", info, err)
	}
}

func TestCallPreviewBrokerAtTimesOutAndWithdraws(t *testing.T) {
	requests, responses := t.TempDir(), t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := callPreviewBrokerAt(ctx, preview.Request{Instance: "task-a", Path: "/w"}, os.Getuid(), requests, responses)
	if err == nil {
		t.Fatal("want timeout")
	}
	if entries, _ := os.ReadDir(requests); len(entries) != 0 {
		t.Errorf("request left behind: %v", entries)
	}
}

// A response not owned by the caller or readable by others is refused.
func TestReadPreviewResponseChecksPermissions(t *testing.T) {
	p := filepath.Join(t.TempDir(), "r.json")
	if err := os.WriteFile(p, []byte(`{"ok":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readPreviewResponse(p, os.Getuid()); err == nil {
		t.Error("0644 response should fail the ownership checks")
	}
	if err := os.Chmod(p, 0o400); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readPreviewResponse(p, os.Getuid()+1); err == nil {
		t.Error("response owned by another uid should fail")
	}
	if res, ok, err := readPreviewResponse(p, os.Getuid()); err != nil || !ok || !res.OK {
		t.Errorf("valid response: %+v %v %v", res, ok, err)
	}
}

func mustOnly(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("want exactly one file in %s: %v %v", dir, entries, err)
	}
	return entries[0].Name()
}
