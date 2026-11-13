package preview

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var pngMagic = "\x89PNG\r\n\x1a\n"

func TestRequestClean(t *testing.T) {
	good, err := Request{Instance: "task-a", Path: "dist"}.Clean()
	if err != nil || good.Page != "/" || good.Width != DefaultWidth || good.Height != DefaultHeight {
		t.Fatalf("defaults: %+v, %v", good, err)
	}
	for name, r := range map[string]Request{
		"no instance":   {Path: "dist"},
		"slash in name": {Instance: "task/a", Path: "dist"},
		"no path":       {Instance: "task-a"},
		"page not path": {Instance: "task-a", Path: "d", Page: "http://example.com/"},
		"page //":       {Instance: "task-a", Path: "d", Page: "//example.com/"},
		"too wide":      {Instance: "task-a", Path: "d", Width: MaxWidth + 1},
		"too small":     {Instance: "task-a", Path: "d", Width: 10, Height: 10},
	} {
		if _, err := r.Clean(); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestResolveDirStaysInWorkspace(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ws, "dist"), filepath.Join(ws, "inside")); err != nil {
		t.Fatal(err)
	}
	for _, ok := range []string{"dist", filepath.Join(ws, "dist"), "inside", "."} {
		if _, err := ResolveDir(ws, ok); err != nil {
			t.Errorf("ResolveDir(%q): %v", ok, err)
		}
	}
	for name, bad := range map[string]string{
		"parent":        "..",
		"dotdot escape": "dist/../../x",
		"symlink out":   "escape",
		"absolute out":  outside,
		"a file":        "file.txt",
		"missing":       "nope",
	} {
		if _, err := ResolveDir(ws, bad); err == nil {
			t.Errorf("%s (%q): want error", name, bad)
		}
	}
}

func TestHandlerServesFolderReadOnlyAndCannotFollowLinksOut(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>hi</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	h, err := Handler(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	get := func(method, p string) (int, string) {
		req, _ := http.NewRequest(method, srv.URL+p, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		buf := make([]byte, 64)
		n, _ := resp.Body.Read(buf)
		return resp.StatusCode, string(buf[:n])
	}
	if code, body := get("GET", "/"); code != 200 || !strings.Contains(body, "hi") {
		t.Errorf("index: %d %q", code, body)
	}
	if code, body := get("GET", "/link"); code == 200 || strings.Contains(body, "secret") {
		t.Errorf("symlink out was served: %d %q", code, body)
	}
	if code, _ := get("POST", "/"); code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", code)
	}
}

func TestFindChrome(t *testing.T) {
	home := t.TempDir()
	if _, err := FindChrome(home, ""); err == nil || !strings.Contains(err.Error(), ChromiumRevision) {
		t.Fatalf("missing browser: %v", err)
	}
	want := filepath.Join(home, ".cache", "ms-playwright", "chromium_headless_shell-"+ChromiumRevision, "chrome-headless-shell-linux64", "chrome-headless-shell")
	if err := os.MkdirAll(filepath.Dir(want), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A different revision is not the pinned one.
	other := filepath.Join(home, ".cache", "ms-playwright", "chromium_headless_shell-9999", "x", "chrome-headless-shell")
	_ = os.MkdirAll(filepath.Dir(other), 0o755)
	_ = os.WriteFile(other, []byte("x"), 0o755)
	if got, err := FindChrome(home, ""); err != nil || got != want {
		t.Fatalf("FindChrome = %q, %v; want %q", got, err, want)
	}
	if _, err := FindChrome(home, filepath.Join(home, "nope")); err == nil {
		t.Error("missing override should fail")
	}
}

func TestChromeArgsHaveNoWayOut(t *testing.T) {
	args := strings.Join(ChromeArgs("/p", "/o.png", "http://127.0.0.1:1/", 800, 600), " ")
	for _, want := range []string{"--proxy-server=http://127.0.0.1:9", "--proxy-bypass-list=127.0.0.1", "--window-size=800,600", "--screenshot=/o.png", "--headless"} {
		if !strings.Contains(args, want) {
			t.Errorf("missing %q in %s", want, args)
		}
	}
}

// fakeChrome stands in for the browser: it fetches the page from the
// capture's own server (proving the folder is served) and writes the output.
func fakeChrome(body string, gotPage *string) RunFunc {
	return func(ctx context.Context, runUser, chrome string, args []string) ([]byte, error) {
		var out, page string
		for _, a := range args {
			if v, ok := strings.CutPrefix(a, "--screenshot="); ok {
				out = v
			} else if strings.HasPrefix(a, "http://") {
				page = a
			}
		}
		*gotPage = page
		resp, err := http.Get(page)
		if err != nil {
			return []byte(err.Error()), err
		}
		resp.Body.Close()
		return nil, os.WriteFile(out, []byte(body), 0o600)
	}
}

func captureOpts(t *testing.T, ws string, req Request, run RunFunc) Options {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	return Options{Req: req, Workspace: ws, RunUser: u.Username, Chrome: "chrome", Run: run, Store: filepath.Join(t.TempDir(), "store")}
}

func TestCaptureStoresPNGUnderInstance(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "dist", "index.html"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	var page string
	o := captureOpts(t, ws, Request{Instance: "task-capture-test", Path: "dist", Page: "/index.html"}, fakeChrome(pngMagic+"data", &page))
	res := Capture(context.Background(), o)
	if !res.OK {
		t.Fatalf("capture failed: %+v", res)
	}
	if !strings.HasPrefix(page, "http://127.0.0.1:") || !strings.HasSuffix(page, "/index.html") {
		t.Errorf("browser was pointed at %q", page)
	}
	if filepath.Dir(res.Path) != o.Store {
		t.Errorf("stored at %s, want under %s", res.Path, o.Store)
	}
	if data, err := os.ReadFile(res.Path); err != nil || !strings.HasPrefix(string(data), pngMagic) {
		t.Errorf("stored file: %v", err)
	}
	// No temp or profile litter stays in the store.
	entries, _ := os.ReadDir(filepath.Dir(res.Path))
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "capture-") {
			t.Errorf("leftover %s", e.Name())
		}
	}
}

func TestCaptureRefusals(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	var page string
	cases := map[string]struct {
		req  Request
		run  RunFunc
		want string
	}{
		"outside workspace": {Request{Instance: "task-capture-refuse", Path: "/"}, fakeChrome(pngMagic, &page), "outside the instance workspace"},
		"not a png":         {Request{Instance: "task-capture-refuse", Path: "dist"}, fakeChrome("GIF89a", &page), "not a PNG"},
		"too big":           {Request{Instance: "task-capture-refuse", Path: "dist"}, fakeChrome(pngMagic+strings.Repeat("x", MaxPNGBytes), &page), "over the"},
		"no picture": {Request{Instance: "task-capture-refuse", Path: "dist"}, func(context.Context, string, string, []string) ([]byte, error) {
			return []byte("no display"), nil
		}, "no picture"},
	}
	for name, c := range cases {
		res := Capture(context.Background(), captureOpts(t, ws, c.req, c.run))
		if res.OK || !strings.Contains(res.Error, c.want) {
			t.Errorf("%s: %+v, want error containing %q", name, res, c.want)
		}
	}
	// A hung browser is cut off by the fixed timeout.
	hang := func(ctx context.Context, _, _ string, _ []string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	o := captureOpts(t, ws, Request{Instance: "task-capture-refuse", Path: "dist"}, hang)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if res := Capture(ctx, o); res.OK || !strings.Contains(res.Error, "timed out") {
		t.Errorf("hang: %+v", res)
	}
}

func TestPruneKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"capture-1.png", "capture-2.png", "capture-3.png", "other.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	Prune(dir, 2)
	for n, want := range map[string]bool{"capture-1.png": false, "capture-2.png": true, "capture-3.png": true, "other.txt": true} {
		if _, err := os.Stat(filepath.Join(dir, n)); (err == nil) != want {
			t.Errorf("%s present=%v, want %v", n, err == nil, want)
		}
	}
}
