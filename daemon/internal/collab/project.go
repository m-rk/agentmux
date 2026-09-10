package collab

import (
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
)

type CommandFactory func(name string, args ...string) *exec.Cmd

func DetectProject(workdir, override string, command CommandFactory) (string, error) {
	if override = strings.TrimSpace(override); override != "" {
		if err := ValidateProjectKey(override); err != nil {
			return "", err
		}
		return override, nil
	}
	if command == nil {
		command = exec.Command
	}
	out, err := command("git", "-C", workdir, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", fmt.Errorf("detecting project from git origin in %s: %w; configure an explicit project key", workdir, err)
	}
	project, err := NormalizeGitRemote(string(out))
	if err != nil {
		return "", fmt.Errorf("detecting project from git origin in %s: %w", workdir, err)
	}
	return project, nil
}

func NormalizeGitRemote(remote string) (string, error) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return "", fmt.Errorf("git origin is empty")
	}

	host, repoPath := "", ""
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil || u.Hostname() == "" {
			return "", fmt.Errorf("unsupported git origin %q", remote)
		}
		host = strings.ToLower(u.Hostname())
		repoPath = u.Path
	} else if at := strings.LastIndex(remote, "@"); at >= 0 {
		afterUser := remote[at+1:]
		colon := strings.Index(afterUser, ":")
		if colon <= 0 {
			return "", fmt.Errorf("unsupported git origin %q", remote)
		}
		host = strings.ToLower(afterUser[:colon])
		repoPath = afterUser[colon+1:]
	} else if filepath.IsAbs(remote) || strings.HasPrefix(remote, "./") || strings.HasPrefix(remote, "../") {
		return "", fmt.Errorf("local git origins require an explicit project key")
	} else {
		return "", fmt.Errorf("unsupported git origin %q", remote)
	}

	repoPath = strings.Trim(strings.TrimSuffix(repoPath, ".git"), "/")
	if repoPath == "" {
		return "", fmt.Errorf("git origin has no repository path")
	}
	project := host + "/" + strings.ToLower(repoPath)
	if err := ValidateProjectKey(project); err != nil {
		return "", err
	}
	return project, nil
}
