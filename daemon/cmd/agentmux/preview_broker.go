package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/liveguard"
	"github.com/m-rk/agentmux/daemon/internal/preview"
)

// The preview mailbox mirrors the task-check broker's: a worker in a Codex
// sandbox cannot reach agentmuxd or open any socket, but it can write a small
// file under /tmp. The root broker reads it, runs the capture on the host and
// answers with a response file only the requesting user can read.
const (
	previewBrokerRoot       = "/tmp/agentmux-preview-broker"
	previewBrokerRequests   = previewBrokerRoot + "/requests"
	previewBrokerResponses  = previewBrokerRoot + "/responses"
	previewBrokerProcessing = previewBrokerRoot + "/processing"
)

// runSessionsPreview is `agentmux sessions preview PATH`: ask the host to
// screenshot a static folder the worker built and print the PNG's path.
func runSessionsPreview(args []string) {
	fs := flag.NewFlagSet("sessions preview", flag.ExitOnError)
	page := fs.String("page", "/", "URL path inside the folder to load")
	width := fs.Int("width", preview.DefaultWidth, "viewport width in pixels")
	height := fs.Int("height", preview.DefaultHeight, "viewport height in pixels")
	jsonOut := fs.Bool("json", false, "print machine-readable output")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: agentmux sessions preview [-page /path] [-width N] [-height N] [-json] FOLDER")
		os.Exit(2)
	}
	instance := os.Getenv(liveguard.InstanceEnv)
	if os.Getenv(liveguard.TaskEnv) != "1" || !strings.HasPrefix(instance, liveguard.TaskPrefix) {
		fmt.Fprintln(os.Stderr, "agentmux sessions preview is available only inside an agentmux task session")
		os.Exit(1)
	}
	folder, err := filepath.Abs(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentmux sessions preview: %v\n", err)
		os.Exit(1)
	}
	req := preview.Request{Instance: instance, Path: folder, Page: *page, Width: *width, Height: *height}
	if _, err := req.Clean(); err != nil {
		fmt.Fprintf(os.Stderr, "agentmux sessions preview: %v\n", err)
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), preview.Timeout+30*time.Second)
	defer cancel()
	res, err := callPreviewBroker(ctx, req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentmux sessions preview: broker unavailable: %v\n", err)
		os.Exit(1)
	}
	if *jsonOut {
		_ = json.NewEncoder(os.Stdout).Encode(res)
	} else if res.OK {
		fmt.Println(res.Path)
	} else {
		fmt.Fprintf(os.Stderr, "agentmux sessions preview: %s\n", res.Error)
	}
	if !res.OK {
		os.Exit(1)
	}
}

func callPreviewBroker(ctx context.Context, req preview.Request) (preview.Result, error) {
	if err := validateTaskcheckBrokerDirs(previewBrokerRoot, 0); err != nil {
		return preview.Result{}, fmt.Errorf("preview broker setup: %w", err)
	}
	return callPreviewBrokerAt(ctx, req, os.Getuid(), previewBrokerRequests, previewBrokerResponses)
}

func callPreviewBrokerAt(ctx context.Context, req preview.Request, uid int, requestsDir, responsesDir string) (preview.Result, error) {
	var requestID [16]byte
	if _, err := rand.Read(requestID[:]); err != nil {
		return preview.Result{}, fmt.Errorf("creating request ID: %w", err)
	}
	name := strconv.Itoa(uid) + "-" + hex.EncodeToString(requestID[:]) + ".json"
	requestPath := filepath.Join(requestsDir, name)
	body, err := json.Marshal(req)
	if err != nil {
		return preview.Result{}, err
	}
	f, err := os.OpenFile(requestPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return preview.Result{}, fmt.Errorf("submitting preview request: %w", err)
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		_ = os.Remove(requestPath)
		return preview.Result{}, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(requestPath)
		return preview.Result{}, err
	}
	responsePath := filepath.Join(responsesDir, name)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if res, ok, err := readPreviewResponse(responsePath, uid); err != nil {
			return preview.Result{}, err
		} else if ok {
			return res, nil
		}
		select {
		case <-ctx.Done():
			_ = os.Remove(requestPath)
			return preview.Result{}, fmt.Errorf("waiting for preview broker: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func readPreviewResponse(path string, uid int) (preview.Result, bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return preview.Result{}, false, nil
	}
	if err != nil {
		return preview.Result{}, false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != uid || !info.Mode().IsRegular() || info.Mode().Perm() != 0o400 {
		return preview.Result{}, false, errors.New("preview response failed ownership checks")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return preview.Result{}, false, err
	}
	var res preview.Result
	if err := json.Unmarshal(data, &res); err != nil {
		return preview.Result{}, false, err
	}
	return res, true, nil
}

// decodePreviewRequest reads one small JSON object and nothing else. Unknown
// fields are refused, so the broker takes no command, environment or browser
// flag from a caller.
func decodePreviewRequest(r io.Reader) (preview.Request, error) {
	dec := json.NewDecoder(io.LimitReader(r, 4096))
	dec.DisallowUnknownFields()
	var req preview.Request
	if err := dec.Decode(&req); err != nil {
		return preview.Request{}, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return preview.Request{}, errors.New("preview request must contain exactly one JSON object")
	}
	return req.Clean()
}
