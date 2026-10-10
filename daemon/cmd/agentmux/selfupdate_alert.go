package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/collab"
	"github.com/m-rk/agentmux/daemon/internal/selfupdate"
)

const defaultSelfUpdateFailureThreshold = 3

var postSelfUpdateFailureAskFn = postSelfUpdateFailureAsk

func selfUpdateFailureThreshold() int {
	value := os.Getenv("AGENTMUX_SELF_UPDATE_FAILURE_THRESHOLD")
	if value == "" {
		if home, err := os.UserHomeDir(); err == nil {
			if b, err := os.ReadFile(selfupdate.FailureThresholdPath(home)); err == nil {
				value = strings.TrimSpace(string(b))
			}
		}
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 {
		return defaultSelfUpdateFailureThreshold
	}
	return n
}

func firstErrorLine(s string) string {
	s = strings.TrimSpace(s)
	if line, _, ok := strings.Cut(s, "\n"); ok {
		return strings.TrimSpace(line)
	}
	return s
}

func selfUpdateBackedOff(state selfupdate.Failure, sha string, threshold int) bool {
	return state.SHA == sha && state.Count >= threshold
}

// trackSelfUpdateFailure persists identical consecutive failures and opens
// one ask once the configured threshold is reached. Alert delivery failures
// never interfere with rollback or updater scheduling.
func trackSelfUpdateFailure(home, repo, sha, reason string) {
	if sha == "" || !selfupdate.ValidSHA(sha) {
		return
	}
	states, err := selfupdate.LoadFailures(selfupdate.FailuresPath(home))
	if err != nil {
		return
	}
	line := firstErrorLine(reason)
	key := repo + "@" + sha
	s := states[key]
	if s.SHA == sha && s.Error == line {
		s.Count++
	} else {
		s = selfupdate.Failure{SHA: sha, Error: line, Count: 1, ThreadID: s.ThreadID, Alerted: s.Alerted}
	}
	if s.Count >= selfUpdateFailureThreshold() && !s.Alerted {
		if thread, postErr := postSelfUpdateFailureAskFn(repo, sha, line, s.Count); postErr == nil {
			s.ThreadID = thread
		}
		s.Alerted = true
	}
	states[key] = s
	_ = selfupdate.SaveFailures(selfupdate.FailuresPath(home), states)
}

func clearSelfUpdateFailure(home, repo string) {
	states, err := selfupdate.LoadFailures(selfupdate.FailuresPath(home))
	if err != nil {
		return
	}
	for key, s := range states {
		if !strings.HasPrefix(key, repo+"@") {
			continue
		}
		if s.ThreadID != "" {
			client, clientErr := asksClient()
			if clientErr == nil {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				_ = client.CloseAsk(ctx, s.ThreadID, "done", true)
				cancel()
			}
		}
		s.ThreadID = ""
		states[key] = s
	}
	_ = selfupdate.SaveFailures(selfupdate.FailuresPath(home), states)
}

func postSelfUpdateFailureAsk(repo, sha, firstLine string, count int) (string, error) {
	client, err := asksClient()
	if err != nil {
		return "", err
	}
	host := address.LocalHostName()
	body := fmt.Sprintf("Host: %s\nRepo: %s\nSHA: %s\nRepeated identical failures: %d\nFirst error: %s", host, repo, sha, count, firstLine)
	if repo == "agentmux" {
		body += "\nBootstrap command: bash scripts/bootstrap-mac.sh"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	thread, _, err := client.PostAsk(ctx, "Self-update repeatedly failing on "+host, body, nil, collab.AskOptions{})
	if thread != "" {
		return thread, nil
	}
	return "", err
}
