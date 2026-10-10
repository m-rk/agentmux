package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// resolveSelfUpdateGo pins Go to an absolute executable path. First honor
// the installer's PATH, then check conventional macOS install locations so
// existing launchd jobs without a pinned value can recover at runtime.
func resolveSelfUpdateGo(configured, path string) (string, error) {
	candidates := []string{configured}
	if configured == "" {
		for _, dir := range filepath.SplitList(path) {
			if dir != "" {
				candidates = append(candidates, filepath.Join(dir, "go"))
			}
		}
	}
	candidates = append(candidates,
		"/opt/homebrew/bin/go",
		"/usr/local/go/bin/go",
		"/usr/local/bin/go",
		"/opt/local/bin/go",
		"/usr/bin/go",
	)
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		abs, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			continue
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		return resolved, nil
	}
	if configured != "" {
		return "", fmt.Errorf("configured Go executable %q is unavailable; reinstall with -go-bin PATH or ensure Go is installed", configured)
	}
	return "", fmt.Errorf("Go executable not found; install Go or reinstall with -go-bin PATH")
}
