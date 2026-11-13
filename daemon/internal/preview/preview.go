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
	"io"
	"net"
	"net/http"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"sort"
	"strconv"
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

// Handler serves root read-only. It cannot follow a symlink out of root.
func Handler(root *os.Root) http.Handler {
	files := http.FileServerFS(root.FS())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "read-only", http.StatusMethodNotAllowed)
			return
		}
		files.ServeHTTP(w, r)
	})
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

// StoreBase is the root-owned directory the broker keeps captures in, one
// subdirectory per instance. It is never under a user-owned path, so a task
// user cannot point it somewhere else; every operation inside it goes through
// an os.Root, which refuses any symlink that leaves the store.
const StoreBase = "/var/lib/agentmux/preview"

// StoreDir is where an instance's captures are, under base.
func StoreDir(base, instance string) string { return filepath.Join(base, instance) }

// openStore returns a handle on base/instance, creating the instance directory
// (0755, so the worker can read the PNGs) when absent. It refuses a base that
// is, or sits under, a symlink, and an instance entry that is not a plain
// directory, before touching anything.
func openStore(base, instance string) (*os.Root, error) {
	if strings.ContainsAny(instance, "/\\\x00") || instance == "" || instance == "." || instance == ".." {
		return nil, errors.New("invalid instance name")
	}
	base = filepath.Clean(base)
	if err := mkdirNoFollow(base); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, fmt.Errorf("opening capture store: %w", err)
	}
	defer root.Close()
	if info, err := root.Lstat(instance); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("instance capture directory is not a plain directory; refusing")
		}
	} else if os.IsNotExist(err) {
		if err := root.Mkdir(instance, 0o755); err != nil {
			return nil, fmt.Errorf("creating instance capture directory: %w", err)
		}
	} else {
		return nil, err
	}
	return root.OpenRoot(instance)
}

// mkdirNoFollow creates dir and any missing parents, walking from the root and
// refusing the first component that is a symlink (or not a directory) before
// creating anything beneath it. MkdirAll would follow such a link and create
// directories wherever it points.
func mkdirNoFollow(dir string) error {
	if !filepath.IsAbs(dir) {
		return errors.New("capture store must be an absolute path")
	}
	cur := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(dir, cur), cur) {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		switch {
		case err == nil:
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fmt.Errorf("capture store path %s is a symlink or not a directory; refusing", cur)
			}
		case os.IsNotExist(err):
			if err := os.Mkdir(cur, 0o755); err != nil {
				return fmt.Errorf("creating capture store: %w", err)
			}
		default:
			return fmt.Errorf("checking capture store: %w", err)
		}
	}
	return nil
}

// RemoveStore deletes instance's captures under base without following any
// symlink out of base. A store that does not exist is not an error.
func RemoveStore(base, instance string) error {
	if strings.ContainsAny(instance, "/\\\x00") || instance == "" || instance == "." || instance == ".." {
		return errors.New("invalid instance name")
	}
	root, err := os.OpenRoot(filepath.Clean(base))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer root.Close()
	if info, err := root.Lstat(instance); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return root.Remove(instance) // drop the link itself, never its target
	}
	return root.RemoveAll(instance)
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
	// StoreBase overrides the root-owned capture store (the StoreBase
	// constant when empty). Tests use it.
	StoreBase string
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
	base := o.StoreBase
	if base == "" {
		base = StoreBase
	}
	store, err := openStore(base, req.Instance)
	if err != nil {
		return fail("%v", err)
	}
	defer store.Close()
	work, err := pinnedRoot(dir)
	if err != nil {
		return fail("opening folder: %v", err)
	}
	defer work.Close()
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fail("listening: %v", err)
	}
	srv := &http.Server{Handler: Handler(work), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(lis) }()
	defer srv.Close()

	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	name := "capture-" + strings.NewReplacer(".", "-").Replace(now().UTC().Format("20060102T150405.000Z")) + ".png"

	// The browser's scratch space (its profile and the picture it writes) is a
	// directory handed to the run user. The broker only ever reads it back
	// through the store's os.Root, so nothing the user plants there can make
	// root touch a path outside the store.
	scratch := ".scratch-" + strings.TrimSuffix(name, ".png")
	if err := store.Mkdir(scratch, 0o700); err != nil {
		return fail("creating browser scratch directory: %v", err)
	}
	defer store.RemoveAll(scratch)
	if err := chownRoot(store, scratch, u); err != nil {
		return fail("handing scratch directory to the run user: %v", err)
	}
	scratchPath := filepath.Join(store.Name(), scratch)
	outFile := "out.png"

	pageURL := "http://" + lis.Addr().String() + path.Clean(req.Page)
	if strings.HasSuffix(req.Page, "/") && !strings.HasSuffix(pageURL, "/") {
		pageURL += "/"
	}
	run := o.Run
	if run == nil {
		run = RunAsUser
	}
	out, err := run(ctx, o.RunUser, o.Chrome, ChromeArgs(filepath.Join(scratchPath, "profile"), filepath.Join(scratchPath, outFile), pageURL, req.Width, req.Height))
	if err != nil {
		if ctx.Err() != nil {
			return fail("capture timed out after %s", Timeout)
		}
		return fail("browser failed: %v: %s", err, firstLine(out))
	}
	png, err := readPicture(store, scratch+"/"+outFile)
	if err != nil {
		return fail("%v: %s", err, firstLine(out))
	}
	f, err := store.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fail("storing picture: %v", err)
	}
	_, werr := f.Write(png)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = store.Remove(name)
		return fail("storing picture: %v", werr)
	}
	Prune(store, Keep)
	return Result{OK: true, Path: filepath.Join(store.Name(), name), Bytes: int64(len(png)), Width: req.Width, Height: req.Height, Millis: time.Since(started).Milliseconds()}
}

// readPicture reads the browser's output through root, so a symlink the run
// user left in its scratch directory cannot lead the broker to another file.
// It checks the size and the PNG signature.
func readPicture(root *os.Root, rel string) ([]byte, error) {
	f, err := root.Open(rel)
	if err != nil {
		return nil, errors.New("browser produced no picture")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("browser produced no picture")
	}
	if info.Size() > MaxPNGBytes {
		return nil, fmt.Errorf("picture is %d bytes, over the %d byte limit", info.Size(), MaxPNGBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxPNGBytes+1))
	if err != nil || int64(len(data)) > MaxPNGBytes {
		return nil, errors.New("picture is over the size limit")
	}
	if !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
		return nil, errors.New("browser output is not a PNG")
	}
	return data, nil
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
func Prune(store *os.Root, keep int) {
	dir, err := store.Open(".")
	if err != nil {
		return
	}
	entries, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil {
		return
	}
	var caps []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "capture-") && strings.HasSuffix(e.Name(), ".png") {
			caps = append(caps, e.Name())
		}
	}
	sort.Strings(caps)
	for len(caps) > keep {
		_ = store.Remove(caps[0])
		caps = caps[1:]
	}
}

// chownRoot hands the directory rel under root to u. It opens the directory
// through root and uses fchown, so no path is followed.
func chownRoot(root *os.Root, rel string, u *user.User) error {
	if os.Geteuid() != 0 {
		return nil
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return err
	}
	d, err := root.Open(rel)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Chown(uid, gid)
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
