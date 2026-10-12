//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/session"
	"github.com/m-rk/agentmux/daemon/internal/taskcheck"
)

var brokerRequestNameRE = regexp.MustCompile(`^([1-9][0-9]*)-([0-9a-f]{32})\.json$`)

func serveTaskcheckBroker() error {
	if os.Geteuid() != 0 {
		return errors.New("task-check broker must run as root")
	}
	if err := ensureTaskcheckBrokerDirs(); err != nil {
		return err
	}
	active := make(chan struct{}, 4)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		entries, err := os.ReadDir(taskcheckBrokerRequests)
		if err != nil {
			return fmt.Errorf("reading broker requests: %w", err)
		}
		for _, entry := range entries {
			if len(active) >= cap(active) {
				break
			}
			active <- struct{}{}
			name := entry.Name()
			if !brokerRequestNameRE.MatchString(name) {
				<-active
				continue
			}
			processingPath := filepath.Join(taskcheckBrokerProcessing, name)
			if err := os.Rename(filepath.Join(taskcheckBrokerRequests, name), processingPath); err != nil {
				<-active
				continue
			}
			go func(name, path string) {
				defer func() { <-active }()
				defer os.Remove(path)
				processTaskcheckRequest(name, path)
			}(name, processingPath)
		}
		cleanupOldBrokerResponses()
	}
	return nil
}

func ensureTaskcheckBrokerDirs() error {
	for _, dir := range []struct {
		path string
		mode os.FileMode
	}{
		{taskcheckBrokerRoot, 0o755},
		{taskcheckBrokerRequests, os.ModeSticky | 0o733},
		{taskcheckBrokerResponses, 0o711},
		{taskcheckBrokerProcessing, 0o700},
	} {
		if err := os.Mkdir(dir.path, dir.mode); err != nil && !os.IsExist(err) {
			return fmt.Errorf("creating broker directory: %w", err)
		}
		info, err := os.Lstat(dir.path)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("broker directory %q has unsafe ownership or type", dir.path)
		}
		if err := os.Chmod(dir.path, dir.mode); err != nil {
			return fmt.Errorf("securing broker directory %q: %w", dir.path, err)
		}
	}
	return validateTaskcheckBrokerDirs(taskcheckBrokerRoot, 0)
}

func processTaskcheckRequest(name, path string) {
	match := brokerRequestNameRE.FindStringSubmatch(name)
	if len(match) != 3 {
		return
	}
	uid, err := strconv.Atoi(match[1])
	if err != nil || uid == 0 {
		return
	}
	file, err := openOwnedRequest(path, uint32(uid))
	if err != nil {
		return
	}
	request, err := decodeTaskcheckRequest(io.LimitReader(file, 1024))
	_ = file.Close()
	var result taskcheck.Result
	if err != nil {
		result = taskcheck.Result{Category: "execution_error", ExitCode: -1, Output: "invalid broker request\n"}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		result = taskcheck.Broker(ctx, request.Instance, strconv.Itoa(uid), session.ReadRegistry,
			func(name string) (string, error) {
				registered, err := user.Lookup(name)
				if err != nil {
					return "", err
				}
				return registered.Uid, nil
			}, nil)
		cancel()
	}
	_ = writeTaskcheckResponse(name, uid, result)
}

func openOwnedRequest(path string, uid uint32) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uid || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		_ = file.Close()
		return nil, errors.New("request ownership check failed")
	}
	return file, nil
}

func writeTaskcheckResponse(name string, uid int, result taskcheck.Result) error {
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(taskcheckBrokerResponses, ".response-*")
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
	if err := os.Rename(tmpPath, filepath.Join(taskcheckBrokerResponses, name)); err != nil {
		return err
	}
	return nil
}

func cleanupOldBrokerResponses() {
	entries, err := os.ReadDir(taskcheckBrokerResponses)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-2 * time.Hour)
	for _, entry := range entries {
		if !brokerRequestNameRE.MatchString(entry.Name()) {
			continue
		}
		path := filepath.Join(taskcheckBrokerResponses, entry.Name())
		info, err := os.Lstat(path)
		if err == nil && info.Mode().IsRegular() && info.ModTime().Before(cutoff) {
			_ = os.Remove(path)
		}
	}
}
