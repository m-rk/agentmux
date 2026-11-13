// Package preview takes a screenshot of a task worker's static build output
// on the host, outside the worker's sandbox.
//
// Codex's task sandbox denies a worker TCP sockets, and headless Chromium
// aborts inside it, so a worker cannot screenshot its own preview. A worker
// instead asks the host (see cmd/agentmux's preview broker) to capture a
// folder it built. This package is the capture itself: it serves the folder
// on a loopback port the browser alone uses, runs a headless Chromium as the
// task's run user, and stores the PNG under the task instance.
//
// Limits are fixed here, not caller-controlled: the folder must resolve to a
// directory inside the instance's own workspace (symlinks included), the
// picture and the wait are capped, and the browser is pointed at a dead proxy
// so it can fetch nothing but that one loopback server.
package preview

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/runas"
)

// ChromiumRevision is the Chromium build the host capture is pinned to. It is
// the revision playwright-core 1.63.0 installs as chromium_headless_shell, so
// anything on the host that drives the same browser through playwright-core
// must keep that package at 1.63.x; bump both together.
const ChromiumRevision = "1243"

// Fixed limits.
const (
	DefaultWidth  = 1280
	DefaultHeight = 800
	MaxWidth      = 2560
	MaxHeight     = 2560
	// Timeout bounds one whole capture, browser start to PNG on disk.
	Timeout = 45 * time.Second
	// MaxPNGBytes caps the stored picture.
	MaxPNGBytes = 8 << 20
	// Keep is how many captures an instance retains; older ones are pruned.
	Keep = 20
)

// Request is one capture, as the worker asked for it.
type Request struct {
	Instance string `json:"instance"`
	// Path is the folder to serve: absolute, or relative to the instance's
	// workspace. It must resolve inside the workspace.
	Path string `json:"path"`
	// Page is the URL path to load, default "/" (the folder's index.html).
	Page   string `json:"page,omitempty"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}

// Result is the outcome handed back to the worker.
type Result struct {
	OK     bool   `json:"ok"`
	Path   string `json:"path,omitempty"`
	Bytes  int64  `json:"bytes,omitempty"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
	Error  string `json:"error,omitempty"`
	Millis int64  `json:"duration_ms,omitempty"`
}

// Clean validates a request's shape and fills the defaults.
func (r Request) Clean() (Request, error) {
	if r.Instance == "" || strings.ContainsAny(r.Instance, "/\\\x00\r\n") {
		return r, errors.New("missing or invalid instance")
	}
	if r.Path == "" || strings.ContainsAny(r.Path, "\x00\r\n") {
		return r, errors.New("missing or invalid path")
	}
	if r.Page == "" {
		r.Page = "/"
	}
	if !strings.HasPrefix(r.Page, "/") || strings.ContainsAny(r.Page, "\x00\r\n \\") || strings.Contains(r.Page, "//") {
		return r, errors.New("page must be a plain URL path such as /index.html")
	}
	if r.Width == 0 {
		r.Width = DefaultWidth
	}
	if r.Height == 0 {
		r.Height = DefaultHeight
	}
	if r.Width < 100 || r.Width > MaxWidth || r.Height < 100 || r.Height > MaxHeight {
		return r, fmt.Errorf("width and height must be between 100 and %d", MaxWidth)
	}
	return r, nil
}

// ResolveDir returns the real path of p when it is a directory inside
// workspace. Relative paths are taken from the workspace. Symlinks are
// resolved first, so a link that leaves the workspace is refused.
func ResolveDir(workspace, p string) (string, error) {
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", fmt.Errorf("resolving workspace: %w", err)
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(workspace, p)
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(p))
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", p, err)
	}
	rel, err := filepath.Rel(root, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("path is outside the instance workspace")
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("path must be a folder (a static build directory)")
	}
	return real, nil
}

// Handler serves dir read-only. It cannot follow a symlink out of dir.
func Handler(dir string) (http.Handler, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	files := http.FileServerFS(root.FS())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "read-only", http.StatusMethodNotAllowed)
			return
		}
		files.ServeHTTP(w, r)
	}), nil
}

// FindChrome returns the pinned headless Chromium under home. override, when
// set (a host's AGENTMUX_PREVIEW_CHROME), wins and must exist.
func FindChrome(home, override string) (string, error) {
	if override != "" {
		if _, err := os.Stat(override); err != nil {
			return "", fmt.Errorf("AGENTMUX_PREVIEW_CHROME: %w", err)
		}
		return override, nil
	}
	base := filepath.Join(home, ".cache", "ms-playwright")
	for _, pattern := range []string{
		filepath.Join(base, "chromium_headless_shell-"+ChromiumRevision, "*", "chrome-headless-shell"),
		filepath.Join(base, "chromium-"+ChromiumRevision, "*", "chrome"),
	} {
		matches, _ := filepath.Glob(pattern)
		sort.Strings(matches)
		if len(matches) > 0 {
			return matches[0], nil
		}
	}
	return "", fmt.Errorf("headless Chromium revision %s not found under %s (install it with playwright-core 1.63.x)", ChromiumRevision, base)
}

// StoreDir is where an instance's captures live under home.
func StoreDir(home, instance string) string {
	return filepath.Join(home, ".agentmux", "preview", instance)
}

// RunFunc runs the browser: chrome with args, as the run user. Tests replace it.
type RunFunc func(ctx context.Context, runUser, chrome string, args []string) ([]byte, error)

// RunAsUser is the real RunFunc.
func RunAsUser(ctx context.Context, runUser, chrome string, args []string) ([]byte, error) {
	cmd := runas.CommandContext(ctx, runUser, chrome, args...)
	return cmd.CombinedOutput()
}

// Options is everything Capture needs that is not in the worker's request.
type Options struct {
	Req       Request
	Workspace string
	RunUser   string
	Chrome    string
	Run       RunFunc
	// Store overrides the capture directory (StoreDir under the run user's
	// home when empty). Tests use it.
	Store string
	// Now stamps the file name; time.Now when nil.
	Now func() time.Time
}

// Capture serves the requested folder, screenshots it and stores the PNG.
func Capture(ctx context.Context, o Options) Result {
	started := time.Now()
	fail := func(format string, a ...any) Result {
		return Result{Error: fmt.Sprintf(format, a...), Millis: time.Since(started).Milliseconds()}
	}
	req, err := o.Req.Clean()
	if err != nil {
		return fail("%v", err)
	}
	dir, err := ResolveDir(o.Workspace, req.Path)
	if err != nil {
		return fail("%v", err)
	}
	u, err := user.Lookup(o.RunUser)
	if err != nil {
		return fail("looking up run user: %v", err)
	}
	store := o.Store
	if store == "" {
		store = StoreDir(u.HomeDir, req.Instance)
	}
	if err := os.MkdirAll(store, 0o755); err != nil {
		return fail("creating capture directory: %v", err)
	}
	chownTo(store, u)
	handler, err := Handler(dir)
	if err != nil {
		return fail("opening folder: %v", err)
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fail("listening: %v", err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(lis) }()
	defer srv.Close()

	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	name := "capture-" + now().UTC().Format("20060102T150405.000Z") + ".png"
	name = strings.ReplaceAll(name, ".", "-")
	name = strings.TrimSuffix(name, "-png") + ".png"
	final := filepath.Join(store, name)
	tmp := filepath.Join(store, ".tmp-"+name)
	defer os.Remove(tmp)

	profile, err := os.MkdirTemp(store, ".profile-")
	if err != nil {
		return fail("creating browser profile: %v", err)
	}
	defer os.RemoveAll(profile)
	chownTo(profile, u)

	pageURL := "http://" + lis.Addr().String() + path.Clean(req.Page)
	if strings.HasSuffix(req.Page, "/") && !strings.HasSuffix(pageURL, "/") {
		pageURL += "/"
	}
	run := o.Run
	if run == nil {
		run = RunAsUser
	}
	out, err := run(ctx, o.RunUser, o.Chrome, ChromeArgs(profile, tmp, pageURL, req.Width, req.Height))
	if err != nil {
		if ctx.Err() != nil {
			return fail("capture timed out after %s", Timeout)
		}
		return fail("browser failed: %v: %s", err, firstLine(out))
	}
	info, err := os.Lstat(tmp)
	if err != nil || !info.Mode().IsRegular() {
		return fail("browser produced no picture: %s", firstLine(out))
	}
	if info.Size() > MaxPNGBytes {
		return fail("picture is %d bytes, over the %d byte limit", info.Size(), MaxPNGBytes)
	}
	head := make([]byte, 8)
	f, err := os.Open(tmp)
	if err != nil {
		return fail("reading picture: %v", err)
	}
	_, _ = f.Read(head)
	_ = f.Close()
	if !bytes.Equal(head, []byte("\x89PNG\r\n\x1a\n")) {
		return fail("browser output is not a PNG")
	}
	if err := os.Rename(tmp, final); err != nil {
		return fail("storing picture: %v", err)
	}
	Prune(store, Keep)
	return Result{OK: true, Path: final, Bytes: info.Size(), Width: req.Width, Height: req.Height, Millis: time.Since(started).Milliseconds()}
}

// ChromeArgs is the browser command line. The dead proxy with only loopback
// bypassed means nothing but the capture's own server can be fetched, whatever
// the page asks for.
func ChromeArgs(profile, out, pageURL string, w, h int) []string {
	return []string{
		"--headless",
		// Chromium's own sandbox needs unprivileged user namespaces, which
		// current Ubuntu disables. The browser runs as the task's unprivileged
		// user, loads only the worker's own static files from loopback, and
		// has no route out, so it runs without its sandbox.
		"--no-sandbox",
		"--disable-gpu",
		"--hide-scrollbars",
		"--no-first-run",
		"--disable-extensions",
		"--disable-background-networking",
		"--user-data-dir=" + profile,
		"--proxy-server=http://127.0.0.1:9",
		"--proxy-bypass-list=127.0.0.1",
		"--virtual-time-budget=4000",
		fmt.Sprintf("--window-size=%d,%d", w, h),
		"--screenshot=" + out,
		pageURL,
	}
}

// Prune removes all but the newest keep captures in store.
func Prune(store string, keep int) {
	entries, err := os.ReadDir(store)
	if err != nil {
		return
	}
	var caps []fs.DirEntry
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "capture-") && strings.HasSuffix(e.Name(), ".png") {
			caps = append(caps, e)
		}
	}
	sort.Slice(caps, func(i, j int) bool { return caps[i].Name() < caps[j].Name() })
	for len(caps) > keep {
		_ = os.Remove(filepath.Join(store, caps[0].Name()))
		caps = caps[1:]
	}
}

func chownTo(p string, u *user.User) {
	if os.Geteuid() != 0 {
		return
	}
	var uid, gid int
	if _, err := fmt.Sscan(u.Uid, &uid); err != nil {
		return
	}
	if _, err := fmt.Sscan(u.Gid, &gid); err != nil {
		return
	}
	_ = os.Chown(p, uid, gid)
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
