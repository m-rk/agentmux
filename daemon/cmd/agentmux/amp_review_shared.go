package main

import (
	"context"
	"log"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/ampexec"
	"github.com/m-rk/agentmux/daemon/internal/session"
	"github.com/m-rk/agentmux/daemon/internal/threadwatch"
)

// ampAPIKey finds the optional amp access token: AMP_API_KEY from the
// environment, else review.amp.api_key, else review.amp.api_key_ref
// resolved through 1Password — the same precedence and 1Password pattern
// threadwatchAPIKey (threadwatch_cmd.go) uses for the TypeSafe key. It
// returns where the key came from, or why there isn't one, for a log line
// that never includes the key. An empty key with no note-worthy problem is
// not an error: amp falls back to its own stored `amp login` session.
//
// Shared by `agentmux threadwatch review -agent amp` and `agentmux doctor
// -checker amp`, both of which read this from the same threadwatch.yaml
// review.amp block (see docs/thread-watch.md and docs/doctor.md).
func ampAPIKey(ctx context.Context, ac threadwatch.AmpConfig) (key, note string) {
	if v := strings.TrimSpace(os.Getenv("AMP_API_KEY")); v != "" {
		return v, "amp key from AMP_API_KEY"
	}
	switch {
	case ac.KeyProblem != "":
		return "", ac.KeyProblem
	case ac.APIKey != "":
		return ac.APIKey, "amp key from threadwatch.yaml"
	case ac.APIKeyRef != "":
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		v, err := session.ReadOpRef(ctx, ac.APIKeyRef)
		if err != nil {
			return "", err.Error()
		}
		return v, "amp key from " + ac.APIKeyRef
	}
	return "", "no amp key configured; relying on the amp CLI's own stored login"
}

// ampExecConfig builds an ampexec.Config from threadwatch.yaml's
// review.amp block, filling in the run-time pieces the config file can't
// know itself: the resolved API key (never written to disk, passed to the
// amp child through its environment only — see ampexec.Run) and, for a
// root caller that drops privilege via runas, the target user to chown the
// generated tool-disabling settings file to.
func ampExecConfig(ac threadwatch.AmpConfig, homeDefault, apiKey string, owner *user.User) ampexec.Config {
	workdir := ac.Workdir
	if workdir == "" {
		workdir = homeDefault
	}
	return ampexec.Config{
		Executor:  ac.Executor,
		Workdir:   workdir,
		RunnerDir: ac.RunnerDir,
		Mode:      ac.Mode,
		Label:     ac.Label,
		APIKey:    apiKey,
		Owner:     owner,
	}
}

// logAmpRunnerWarning logs, once at the calling command's own startup, the
// prompt-injection warning a "runner:<id>" amp executor carries: tools
// cannot be disabled on a runner from agentmux's side (see ampexec.Run), so
// untrusted transcript/pane excerpts reach a thread that keeps whatever
// tools that runner's own settings allow. tag identifies the caller in the
// log line (e.g. "threadwatch review" or "doctor").
func logAmpRunnerWarning(tag, executor string) {
	if warning, ok := ampexec.RunnerWarning(executor); ok {
		log.Printf("%s: WARNING: %s", tag, warning)
	}
}
