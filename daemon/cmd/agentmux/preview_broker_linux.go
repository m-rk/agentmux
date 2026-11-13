//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/preview"
	"github.com/m-rk/agentmux/daemon/internal/session"
)

// servePreviewBroker is the preview half of the root broker service. The
// captures run in their own goroutines; the preview.Gate keeps it to one per
// instance and a few host-wide.
func servePreviewBroker() error {
	if os.Geteuid() != 0 {
		return errors.New("preview broker must run as root")
	}
	if err := ensurePreviewBrokerDirs(); err != nil {
		return err
	}
	gate := preview.NewGate(2)
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		entries, err := os.ReadDir(previewBrokerRequests)
		if err != nil {
			return fmt.Errorf("reading preview requests: %w", err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if !brokerRequestNameRE.MatchString(name) {
				continue
			}
			processing := filepath.Join(previewBrokerProcessing, name)
			if err := os.Rename(filepath.Join(previewBrokerRequests, name), processing); err != nil {
				continue
			}
			go func(name, path string) {
				defer os.Remove(path)
				processPreviewRequest(gate, name, path)
			}(name, processing)
		}
		cleanupOldPreviewResponses()
	}
	return nil
}

func ensurePreviewBrokerDirs() error {
	for _, dir := range []struct {
		path string
		mode os.FileMode
	}{
		{previewBrokerRoot, 0o755},
		{previewBrokerRequests, os.ModeSticky | 0o733},
		{previewBrokerResponses, 0o711},
		{previewBrokerProcessing, 0o700},
	} {
		if err := os.Mkdir(dir.path, dir.mode); err != nil && !os.IsExist(err) {
			return fmt.Errorf("creating preview broker directory: %w", err)
		}
		if err := os.Chmod(dir.path, dir.mode); err != nil {
			return fmt.Errorf("securing preview broker directory %q: %w", dir.path, err)
		}
	}
	return validateTaskcheckBrokerDirs(previewBrokerRoot, 0)
}

func processPreviewRequest(gate *preview.Gate, name, path string) {
	match := brokerRequestNameRE.FindStringSubmatch(name)
	if len(match) != 3 {
		return
	}
	uid, err := strconv.Atoi(match[1])
	if err != nil || uid == 0 {
		return
	}
	reply := func(res preview.Result) {
		if err := writePreviewResponse(name, uid, res); err != nil {
			log.Printf("preview broker: writing response: %v", err)
		}
	}
	file, err := openOwnedRequest(path, uint32(uid))
	if err != nil {
		return
	}
	req, err := decodePreviewRequest(file)
	_ = file.Close()
	if err != nil {
		reply(preview.Result{Error: "invalid preview request: " + err.Error()})
		return
	}
	target, err := preview.Authorize(req.Instance, strconv.Itoa(uid), session.ReadRegistry, func(name string) (string, error) {
		u, err := user.Lookup(name)
		if err != nil {
			return "", err
		}
		return u.Uid, nil
	})
	if err != nil {
		reply(preview.Result{Error: err.Error()})
		return
	}
	release, why := gate.Acquire(req.Instance)
	if release == nil {
		reply(preview.Result{Error: why})
		return
	}
	defer release()
	u, err := user.Lookup(target.RunUser)
	if err != nil {
		reply(preview.Result{Error: "looking up task user: " + err.Error()})
		return
	}
	chrome, err := preview.FindChrome(u.HomeDir, os.Getenv("AGENTMUX_PREVIEW_CHROME"))
	if err != nil {
		reply(preview.Result{Error: err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), preview.Timeout+5*time.Second)
	defer cancel()
	res := preview.Capture(ctx, preview.Options{Req: req, Workspace: target.Workspace, RunUser: target.RunUser, Chrome: chrome})
	log.Printf("preview broker: instance=%s ok=%v bytes=%d ms=%d", req.Instance, res.OK, res.Bytes, res.Millis)
	reply(res)
}

func writePreviewResponse(name string, uid int, res preview.Result) error {
	data, err := json.Marshal(res)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(previewBrokerResponses, ".response-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chown(uid, -1); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o400); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, filepath.Join(previewBrokerResponses, name))
}

func cleanupOldPreviewResponses() {
	entries, err := os.ReadDir(previewBrokerResponses)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-2 * time.Hour)
	for _, entry := range entries {
		if !brokerRequestNameRE.MatchString(entry.Name()) {
			continue
		}
		p := filepath.Join(previewBrokerResponses, entry.Name())
		if info, err := os.Lstat(p); err == nil && info.Mode().IsRegular() && info.ModTime().Before(cutoff) {
			_ = os.Remove(p)
		}
	}
}
