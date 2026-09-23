// runAuthCmd is `agentmux auth <status|login>`: headless Claude Code
// re-authentication for browser-less hosts. `claude auth login` prints an
// OAuth authorize URL and waits on a "Paste code here" prompt; this command
// surfaces that URL so an operator can complete it on another computer and
// paste the code back. Local-host only, like `doctor`: both the login child
// and the expiry/status checks exec `claude` as a local OS user.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/m-rk/agentmux/daemon/internal/claudeauth"
	"github.com/m-rk/agentmux/daemon/internal/discovery"
	"github.com/m-rk/agentmux/daemon/internal/provision"
	"github.com/m-rk/agentmux/daemon/internal/session"
)

// loginOverallTimeout bounds the whole interactive login (enough to switch
// to another computer, open the URL, and paste codes back); loginURLTimeout
// bounds just the wait for claude to print its URL.
const (
	loginOverallTimeout = 10 * time.Minute
	loginURLTimeout     = 60 * time.Second
	loginCodeTimeout    = 60 * time.Second
	loginMaxCodeTries   = 5
)

func runAuthCmd(args []string) {
	if len(args) == 0 {
		authUsage()
		os.Exit(1)
	}
	switch args[0] {
	case "status":
		runAuthStatusCmd(args[1:])
	case "login":
		runAuthLoginCmd(args[1:])
	case "-h", "--help", "help":
		authUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown auth subcommand %q\n", args[0])
		authUsage()
		os.Exit(1)
	}
}

func authUsage() {
	fmt.Fprintln(os.Stderr, `usage:
  agentmux auth status [-instance NAME] [-run-user USER] [-all] [-json]
  agentmux auth login [-instance NAME] [-run-user USER] [-method claudeai|console] [-force]`)
}

// authStatusResult is one user's check, for -json output.
type authStatusResult struct {
	RunUser          string `json:"run_user"`
	LoggedIn         bool   `json:"logged_in"`
	AuthMethod       string `json:"auth_method,omitempty"`
	ExpirySupported  bool   `json:"expiry_supported"`
	AccessExpiresAt  *string `json:"access_expires_at,omitempty"`
	RefreshExpiresAt *string `json:"refresh_expires_at,omitempty"`
	Error            string `json:"error,omitempty"`
}

func runAuthStatusCmd(args []string) {
	fs := flag.NewFlagSet("auth status", flag.ExitOnError)
	instance := fs.String("instance", "", "check the run user of this instance (from its registry)")
	runUserFlag := fs.String("run-user", "", "check this OS user directly")
	all := fs.Bool("all", false, "check every distinct claude-code run user on this host")
	jsonOut := fs.Bool("json", false, "print machine-readable JSON instead of text")
	fs.Parse(args)

	if *all && (*instance != "" || *runUserFlag != "") {
		log.Fatal("auth status: -all is mutually exclusive with -instance/-run-user")
	}

	var targets []string
	var hintFor string
	switch {
	case *all:
		var err error
		targets, err = distinctClaudeRunUsers()
		if err != nil {
			log.Fatalf("auth status: %v", err)
		}
		if len(targets) == 0 {
			targets = []string{currentUsername()}
		}
	case *instance != "":
		ru, err := authRunUserForInstance(*instance, *runUserFlag)
		if err != nil {
			log.Fatalf("auth status: %v", err)
		}
		targets = []string{ru}
		hintFor = fmt.Sprintf("-instance %s", *instance)
	default:
		if *runUserFlag != "" {
			if _, err := user.Lookup(*runUserFlag); err != nil {
				log.Fatalf("auth status: looking up user %q: %v", *runUserFlag, err)
			}
			targets = []string{*runUserFlag}
		} else {
			targets = []string{currentUsername()}
		}
	}

	results := make([]authStatusResult, 0, len(targets))
	for _, ru := range targets {
		results = append(results, checkOneAuth(ru))
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(results); err != nil {
			log.Fatalf("auth status: %v", err)
		}
	} else {
		for _, r := range results {
			fmt.Println(formatAuthStatus(r, hintFor))
		}
	}

	for _, r := range results {
		if r.Error != "" || !r.LoggedIn {
			os.Exit(1)
		}
	}
}

// checkOneAuth runs the logged-in check plus the expiry read for one user.
// A claude-side failure to confirm login is reported as logged-out, not an
// error; only a broken invocation (missing binary, unknown user) is an error.
func checkOneAuth(runUser string) authStatusResult {
	r := authStatusResult{RunUser: runUser}
	loggedIn, method, err := claudeauth.CheckLoggedIn(runUser)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	r.LoggedIn = loggedIn
	r.AuthMethod = method
	status, err := provision.CheckTokenExpiry("claude-code", runUser)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	r.ExpirySupported = status.Supported
	if !status.AccessExpiresAt.IsZero() {
		s := status.AccessExpiresAt.Local().Format(time.RFC3339)
		r.AccessExpiresAt = &s
	}
	if !status.RefreshExpiresAt.IsZero() {
		s := status.RefreshExpiresAt.Local().Format(time.RFC3339)
		r.RefreshExpiresAt = &s
	}
	return r
}

func formatAuthStatus(r authStatusResult, hintFor string) string {
	if r.Error != "" {
		return fmt.Sprintf("%s: check failed: %s", r.RunUser, r.Error)
	}
	hint := hintFor
	if hint == "" {
		hint = fmt.Sprintf("-run-user %s", r.RunUser)
	}
	if !r.LoggedIn {
		method := r.AuthMethod
		if method == "" {
			method = "-"
		}
		return fmt.Sprintf("%s: NOT logged in (method %s). Re-authenticate with: agentmux auth login %s", r.RunUser, method, hint)
	}
	var expiry string
	if r.ExpirySupported {
		if r.RefreshExpiresAt != nil {
			if t, err := time.Parse(time.RFC3339, *r.RefreshExpiresAt); err == nil {
				expiry = ", refresh " + formatAuthExpiry(t)
			}
		}
	} else {
		expiry = " (expiry tracking unsupported on this platform; logged-in check only)"
	}
	return fmt.Sprintf("%s: logged in (method %s%s). Refresh early with: agentmux auth login %s -force", r.RunUser, orDash(r.AuthMethod), expiry, hint)
}

// formatAuthExpiry renders a refresh expiry as an absolute time plus a
// relative "in X"/"expired X ago" using this CLI's own humanizeDuration
// (tui.go's relativeTime only handles the past — this covers both sides).
func formatAuthExpiry(t time.Time) string {
	abs := t.Local().Format("2006-01-02 15:04:05")
	d := time.Until(t)
	if d < 0 {
		return fmt.Sprintf("expired %s ago (%s)", humanizeDuration(-d), abs)
	}
	return fmt.Sprintf("expires %s (in %s)", abs, humanizeDuration(d))
}

// distinctClaudeRunUsers returns every distinct run user owning a claude-code
// instance on this host. Instances without a recorded run user (macOS
// registries) fall back to the current user.
func distinctClaudeRunUsers() ([]string, error) {
	instances, err := discovery.List()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, inst := range instances {
		if inst.Agent != "" && inst.Agent != "claude-code" {
			continue
		}
		ru := inst.RunUser
		if ru == "" {
			ru = currentUsername()
		}
		if !seen[ru] {
			seen[ru] = true
			out = append(out, ru)
		}
	}
	return out, nil
}

// authRunUserForInstance resolves -instance (plus an optional explicit
// -run-user override) to the OS user whose claude credentials to act on. A
// registry without AGENTMUX_RUN_USER (macOS) means the current user.
func authRunUserForInstance(instance, override string) (string, error) {
	fields, err := session.ReadRegistry(instance)
	if err != nil {
		return "", fmt.Errorf("reading registry for %q: %w", instance, err)
	}
	recorded := fields["AGENTMUX_RUN_USER"]
	if recorded == "" {
		if override != "" {
			if _, err := user.Lookup(override); err != nil {
				return "", fmt.Errorf("looking up user %q: %w", override, err)
			}
			return override, nil
		}
		return currentUsername(), nil
	}
	if override != "" && override != recorded {
		return "", fmt.Errorf("instance %q runs as %q, which conflicts with -run-user %q", instance, recorded, override)
	}
	return recorded, nil
}

func currentUsername() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if env := os.Getenv("USER"); env != "" {
		return env
	}
	return "unknown"
}

func runAuthLoginCmd(args []string) {
	fs := flag.NewFlagSet("auth login", flag.ExitOnError)
	instance := fs.String("instance", "", "re-authenticate this instance's run user (from its registry)")
	runUserFlag := fs.String("run-user", "", "re-authenticate this OS user directly")
	method := fs.String("method", "claudeai", "login method: claudeai (subscription) or console (API billing)")
	force := fs.Bool("force", false, "re-authenticate even while still logged in (e.g. before expiry)")
	fs.Parse(args)

	var runUser string
	var err error
	if *instance != "" {
		runUser, err = authRunUserForInstance(*instance, *runUserFlag)
	} else if *runUserFlag != "" {
		if _, lookupErr := user.Lookup(*runUserFlag); lookupErr != nil {
			err = fmt.Errorf("looking up user %q: %w", *runUserFlag, lookupErr)
		} else {
			runUser = *runUserFlag
		}
	} else {
		runUser = currentUsername()
	}
	if err != nil {
		log.Fatalf("auth login: %v", err)
	}

	if _, err := claudeauth.LoginMethodArgs(*method); err != nil {
		log.Fatalf("auth login: %v", err)
	}

	if !*force {
		if loggedIn, authMethod, checkErr := claudeauth.CheckLoggedIn(runUser); checkErr == nil && loggedIn {
			fmt.Printf("%s: already logged in (method %s); pass -force to re-authenticate anyway (e.g. before expiry)\n", runUser, orDash(authMethod))
			return
		}
	}

	cmd, err := claudeauth.StartLogin(runUser, *method)
	if err != nil {
		log.Fatalf("auth login: %v", err)
	}
	fmt.Printf("agentmux: starting `claude auth login` as %s (method %s). Complete the URL on another computer, then paste codes back here.\n", runUser, *method)
	if err := runLoginPTY(cmd); err != nil {
		log.Fatalf("auth login: %v", err)
	}

	if loggedIn, authMethod, checkErr := claudeauth.CheckLoggedIn(runUser); checkErr != nil || !loggedIn {
		if checkErr != nil {
			log.Fatalf("auth login: login flow finished but verifying failed: %v", checkErr)
		}
		log.Fatal("auth login: login flow finished but claude still reports logged out")
	} else {
		fmt.Printf("agentmux: %s is logged in (method %s)\n", runUser, orDash(authMethod))
	}
}

// runLoginPTY runs cmd under a PTY, streams its output to stdout, prints the
// authorize URL with other-computer instructions once it appears, and feeds
// pasted codes from stdin back to claude. Pasted codes are written only to
// the PTY — never printed, logged, or included in errors. pty.StartWithAttrs
// (not pty.Start) starts the child: pty.Start would overwrite
// cmd.SysProcAttr with a fresh struct holding only Setsid+Setctty, dropping
// runas's Credential that makes `claude` run as the instance's OS user when
// this command runs as root. The merged attrs keep both.
func runLoginPTY(cmd *exec.Cmd) error {
	attrs := &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if cmd.SysProcAttr != nil {
		merged := *cmd.SysProcAttr
		merged.Setsid = true
		merged.Setctty = true
		attrs = &merged
	}
	f, err := pty.StartWithAttrs(cmd, &pty.Winsize{Cols: 120, Rows: 40}, attrs)
	if err != nil {
		return fmt.Errorf("starting claude auth login under a PTY: %w", err)
	}
	defer func() { _ = f.Close() }()
	printf := func(format string, args ...any) { fmt.Printf(format, args...) }
	return loginSession(f, cmd.Wait, stdinCodeReader, printf)
}

// loginSession drives the PTY exchange once started; split out so the
// scanners stay unit-tested in internal/claudeauth while the terminal
// plumbing lives here with the rest of the CLI.
func loginSession(f *os.File, wait func() error, readCode func() (string, error), printf func(string, ...any)) error {
	var mu sync.Mutex
	var out []byte
	snapshot := func() string {
		mu.Lock()
		defer mu.Unlock()
		return string(out)
	}
	appendChunk := func(b []byte) {
		mu.Lock()
		out = append(out, b...)
		mu.Unlock()
	}

	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				chunk := append([]byte(nil), buf[:n]...)
				appendChunk(chunk)
				_, _ = os.Stdout.Write(chunk)
			}
			if err != nil {
				done <- err
				return
			}
		}
	}()
	waitDone := make(chan error, 1)
	go func() { waitDone <- wait() }()

	deadline := time.Now().Add(loginOverallTimeout)
	urlPrinted := false
	prompted := false
	sentAt := time.Time{}
	invalidSeen := ""
	tries := 0

	urlDeadline := time.Now().Add(loginURLTimeout)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for login to complete", loginOverallTimeout)
		}
		snap := snapshot()
		if !urlPrinted {
			if url := claudeauth.ExtractLoginURL(snap); url != "" {
				printf("\nagentmux: open this URL on another computer and sign in:\n\n  %s\n\nThen paste the code it shows back here.\n", url)
				urlPrinted = true
			} else if time.Now().After(urlDeadline) {
				return fmt.Errorf("timed out waiting for claude to print its login URL: %s", claudeauth.RedactLoginURLs(tailText(snap, 2000)))
			}
		}
		if urlPrinted && !prompted && claudeauth.PromptForCode(snap) {
			printf("agentmux: paste code, then Enter (up to %d tries):\n", loginMaxCodeTries)
			prompted = true
		}
		if claudeauth.LoginSuccessful(snap) {
			return nil
		}
		if prompted && tries < loginMaxCodeTries {
			select {
			case err := <-waitDone:
				if claudeauth.LoginSuccessful(snapshot()) {
					return nil
				}
				if err != nil {
					return fmt.Errorf("claude exited: %v: %s", err, claudeauth.RedactLoginURLs(tailText(snapshot(), 2000)))
				}
				return fmt.Errorf("claude exited before login completed: %s", claudeauth.RedactLoginURLs(tailText(snapshot(), 2000)))
			case err := <-done:
				_ = err // PTY read end; the wait branch reports the real outcome
			case <-time.After(200 * time.Millisecond):
			}
			// Non-blocking stdin check would need a dedicated reader; instead
			// poll prompt state and read a code only when claude is waiting.
			if claudeauth.PromptForCode(snapshot()) && (sentAt.IsZero() || time.Since(sentAt) > 500*time.Millisecond) {
				if sentAt.IsZero() || claudeauth.InvalidCodeReported(snapshot()[len(invalidSeen):]) {
					if !sentAt.IsZero() {
						invalidSeen = snapshot()
						printf("agentmux: that code was rejected — paste another code:\n")
					}
					codeCh := make(chan readResult, 1)
					go func() {
						code, err := readCode()
						codeCh <- readResult{code: code, err: err}
					}()
					select {
					case res := <-codeCh:
						if res.err != nil {
							return fmt.Errorf("reading code: %w", res.err)
						}
						code := strings.TrimSpace(res.code)
						if code == "" {
							continue
						}
						tries++
						sentAt = time.Now()
						if _, err := f.Write([]byte(code + "\n")); err != nil {
							return fmt.Errorf("sending code to claude: %w", err)
						}
						codeDeadline := time.Now().Add(loginCodeTimeout)
						for {
							s := snapshot()
							if claudeauth.LoginSuccessful(s) {
								return nil
							}
							if claudeauth.InvalidCodeReported(s[len(invalidSeen):]) {
								break
							}
							select {
							case err := <-waitDone:
								if claudeauth.LoginSuccessful(snapshot()) {
									return nil
								}
								if err != nil {
									return fmt.Errorf("claude exited: %v: %s", err, claudeauth.RedactLoginURLs(tailText(snapshot(), 2000)))
								}
								return fmt.Errorf("claude exited before login completed: %s", claudeauth.RedactLoginURLs(tailText(snapshot(), 2000)))
							case <-time.After(200 * time.Millisecond):
							}
							if time.Now().After(codeDeadline) {
								return fmt.Errorf("timed out waiting for claude to accept the code: %s", claudeauth.RedactLoginURLs(tailText(snapshot(), 2000)))
							}
						}
					case <-time.After(time.Until(deadline)):
						return fmt.Errorf("timed out after %s waiting for login to complete", loginOverallTimeout)
					}
				}
			}
			continue
		}
		if tries >= loginMaxCodeTries {
			return fmt.Errorf("too many rejected codes (%d); start over with `agentmux auth login`", loginMaxCodeTries)
		}
		select {
		case err := <-waitDone:
			if claudeauth.LoginSuccessful(snapshot()) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("claude exited: %v: %s", err, claudeauth.RedactLoginURLs(tailText(snapshot(), 2000)))
			}
			return fmt.Errorf("claude exited before login completed: %s", claudeauth.RedactLoginURLs(tailText(snapshot(), 2000)))
		case <-time.After(200 * time.Millisecond):
		}
	}
}

type readResult struct {
	code string
	err  error
}

func tailText(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// stdinCodeReader reads one line from stdin for loginSession.
func stdinCodeReader() (string, error) {
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return line, nil
}
