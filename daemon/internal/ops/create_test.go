package ops

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"github.com/m-rk/agentmux/daemon/internal/discovery"
	"github.com/m-rk/agentmux/daemon/internal/pb"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
	"github.com/m-rk/agentmux/daemon/internal/session"
)

type fakeDaemon struct {
	instances []*pb.Instance
	created   []*pb.CreateInstanceRequest
	failWith  string
}

func (f *fakeDaemon) ListInstances(context.Context) ([]*pb.Instance, error) { return f.instances, nil }
func (f *fakeDaemon) Close() error                                          { return nil }
func (f *fakeDaemon) CreateInstance(_ context.Context, req *pb.CreateInstanceRequest) (*pb.CreateInstanceResponse, error) {
	if f.failWith != "" {
		return &pb.CreateInstanceResponse{Message: f.failWith}, nil
	}
	f.created = append(f.created, req)
	f.instances = append(f.instances, &pb.Instance{Name: req.InstanceName, Agent: req.Agent, Provider: req.Provider, Model: req.Model, Workdir: req.Workdir, Status: pb.Status_STATUS_RUNNING})
	// Mirror the daemon: a created instance registers, so later
	// registry writes (AGENTMUX_BRANCH) have a file to update.
	reg := "AGENTMUX_INSTANCE_NAME=" + req.InstanceName + "\nAGENTMUX_AGENT=" + req.Agent + "\nAGENTMUX_WORKDIR=" + req.Workdir + "\n"
	if err := os.WriteFile(filepath.Join(discovery.EnvDir, req.InstanceName+".env"), []byte(reg), 0o644); err != nil {
		return &pb.CreateInstanceResponse{Message: err.Error()}, nil
	}
	return &pb.CreateInstanceResponse{Ok: true, Message: "created"}, nil
}

func (f *fakeDaemon) RetireInstance(_ context.Context, req *pb.RetireInstanceRequest) (*pb.RetireInstanceResponse, error) {
	return &pb.RetireInstanceResponse{Ok: true, Message: "retired " + req.Instance}, nil
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// createEnv sets up a temp repo with a template registry entry and a fake
// daemon.
type createEnv struct {
	env    Env
	d      *fakeDaemon
	repo   string
	wtRoot string
}

func newCreateEnv(t *testing.T, extra ...string) *createEnv {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "app")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "init")

	envDir := filepath.Join(root, "env")
	if err := os.Mkdir(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := discovery.EnvDir
	discovery.EnvDir = envDir
	t.Cleanup(func() { discovery.EnvDir = old })
	lines := []string{"AGENTMUX_AGENT=opencode", "AGENTMUX_WORKDIR=" + repo, "AGENTMUX_PROVIDER=ollama", "AGENTMUX_MODEL=m1",
		"AGENTMUX_PROVIDER_BASE_URL=http://localhost:1/v1", "AGENTMUX_PROVIDER_API_KEY_ENV=KEY_VAR"}
	lines = append(lines, extra...)
	if err := os.WriteFile(filepath.Join(envDir, "tmpl.env"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := &fakeDaemon{}
	return &createEnv{
		env:    Env{Dial: func() (Daemon, error) { return d, nil }},
		d:      d,
		repo:   repo,
		wtRoot: filepath.Join(root, "app-worktrees"),
	}
}

func (c *createEnv) req() CreateRequest {
	return CreateRequest{Template: "tmpl@" + address.LocalHostName(), Instance: "task-1", Branch: "feature/task-1"}
}

func wantReason(t *testing.T, err error, r safesend.Reason) {
	t.Helper()
	if err == nil {
		t.Fatalf("want %s refusal, got success", r)
	}
	if got := AsError(err).Reason; got != r {
		t.Fatalf("reason = %s (%v), want %s", got, err, r)
	}
}

func TestCreateMakesWorktreeAndInstance(t *testing.T) {
	c := newCreateEnv(t)
	res, err := c.env.Create(context.Background(), c.req())
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(c.wtRoot, "task-1")
	if !res.Created || res.Branch != "feature/task-1" || res.Workdir != wt || res.Address != "task-1@"+address.LocalHostName() {
		t.Fatalf("result = %+v", res)
	}
	if got := git(t, wt, "rev-parse", "--abbrev-ref", "HEAD"); got != "feature/task-1" {
		t.Fatalf("worktree branch = %s", got)
	}
	if len(c.d.created) != 1 {
		t.Fatalf("created = %d instances", len(c.d.created))
	}
	r := c.d.created[0]
	if r.InstanceName != "task-1" || r.Agent != "opencode" || r.Provider != "ollama" || r.Model != "m1" ||
		r.ProviderBaseUrl != "http://localhost:1/v1" || r.ProviderApiKeyEnv != "KEY_VAR" || r.Workdir != wt {
		t.Fatalf("CreateInstanceRequest = %+v", r)
	}
	// The branch the worktree was made on is recorded for retire's
	// delete-or-keep decision.
	fields, err := session.ReadRegistry("task-1")
	if err != nil {
		t.Fatalf("reading new registry: %v", err)
	}
	if fields["AGENTMUX_BRANCH"] != "feature/task-1" {
		t.Errorf("AGENTMUX_BRANCH = %q, want the created branch", fields["AGENTMUX_BRANCH"])
	}
}

func TestCreateIsIdempotent(t *testing.T) {
	c := newCreateEnv(t)
	if _, err := c.env.Create(context.Background(), c.req()); err != nil {
		t.Fatal(err)
	}
	res, err := c.env.Create(context.Background(), c.req())
	if err != nil {
		t.Fatal(err)
	}
	if res.Created || len(c.d.created) != 1 {
		t.Fatalf("second create: created=%v, daemon creates=%d", res.Created, len(c.d.created))
	}
}

func TestCreateRefusesInstanceWithOtherWorkdir(t *testing.T) {
	c := newCreateEnv(t)
	c.d.instances = []*pb.Instance{{Name: "task-1", Workdir: "/somewhere/else"}}
	_, err := c.env.Create(context.Background(), c.req())
	wantReason(t, err, safesend.ReasonInvalid)
	if _, serr := os.Stat(c.wtRoot); serr == nil {
		t.Fatal("worktree was created before the name clash was refused")
	}
}

func TestCreateRefusesPathOnOtherBranch(t *testing.T) {
	c := newCreateEnv(t)
	if _, err := c.env.Create(context.Background(), c.req()); err != nil {
		t.Fatal(err)
	}
	r := c.req()
	r.Instance, r.Worktree = "task-2", "task-1" // same path, new branch
	r.Branch = "feature/other"
	_, err := c.env.Create(context.Background(), r)
	wantReason(t, err, safesend.ReasonInvalid)

	// A plain directory is not a worktree either.
	if err := os.Mkdir(filepath.Join(c.wtRoot, "plain"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.Worktree = "plain"
	_, err = c.env.Create(context.Background(), r)
	wantReason(t, err, safesend.ReasonInvalid)
}

func TestCreateUsesExistingBranch(t *testing.T) {
	c := newCreateEnv(t)
	git(t, c.repo, "branch", "feature/task-1")
	if _, err := c.env.Create(context.Background(), c.req()); err != nil {
		t.Fatal(err)
	}
	// The branch is now checked out; another worktree can't take it.
	r := c.req()
	r.Instance = "task-2"
	_, err := c.env.Create(context.Background(), r)
	wantReason(t, err, safesend.ReasonInvalid)
}

// originFor adds a bare "origin" with a main branch holding the repo's
// current commit, and returns a second clone used to push new commits to it.
func originFor(t *testing.T, c *createEnv) (origin, pusher string) {
	t.Helper()
	root := filepath.Dir(c.repo)
	origin = filepath.Join(root, "origin.git")
	git(t, root, "clone", "-q", "--bare", c.repo, origin)
	git(t, c.repo, "remote", "add", "origin", origin)
	git(t, c.repo, "fetch", "-q", "origin")
	pusher = filepath.Join(root, "pusher")
	git(t, root, "clone", "-q", origin, pusher)
	return origin, pusher
}

func TestCreateBaseFetchesOriginBranch(t *testing.T) {
	c := newCreateEnv(t)
	_, pusher := originFor(t, c)
	// origin/release is new and origin/main moves on; neither is known locally yet.
	git(t, pusher, "checkout", "-q", "-b", "release")
	git(t, pusher, "commit", "-q", "--allow-empty", "-m", "release work")
	git(t, pusher, "push", "-q", "origin", "release")
	want := git(t, pusher, "rev-parse", "HEAD")
	// A stale local ref of the same name must not be used.
	git(t, c.repo, "branch", "release", "HEAD")

	r := c.req()
	r.Base = "release"
	res, err := c.env.Create(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if res.Base != "release" || res.BaseCommit != want {
		t.Fatalf("base = %q @ %q, want release @ %s", res.Base, res.BaseCommit, want)
	}
	if got := git(t, filepath.Join(c.wtRoot, "task-1"), "rev-parse", "HEAD"); got != want {
		t.Fatalf("worktree at %s, want origin/release %s", got, want)
	}

	// An origin/ prefix is accepted.
	git(t, pusher, "commit", "-q", "--allow-empty", "-m", "more")
	git(t, pusher, "push", "-q", "origin", "release")
	want = git(t, pusher, "rev-parse", "HEAD")
	r.Instance, r.Branch, r.Base = "task-2", "feature/two", "origin/release"
	res, err = c.env.Create(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if res.Base != "release" || res.BaseCommit != want {
		t.Fatalf("base = %q @ %q, want release @ %s", res.Base, res.BaseCommit, want)
	}
}

func TestCreateBaseRefusesWithoutFallback(t *testing.T) {
	c := newCreateEnv(t)
	r := c.req()
	r.Base = "main" // local main exists, but there is no origin to fetch from
	_, err := c.env.Create(context.Background(), r)
	wantReason(t, err, safesend.ReasonFailed)
	if !strings.Contains(AsError(err).Detail, "main") {
		t.Fatalf("detail = %q", AsError(err).Detail)
	}

	originFor(t, c)
	r.Base = "no-such-branch"
	_, err = c.env.Create(context.Background(), r)
	wantReason(t, err, safesend.ReasonFailed)

	if _, statErr := os.Stat(filepath.Join(c.wtRoot, "task-1")); statErr == nil {
		t.Fatal("a refused request made a worktree")
	}
	if len(c.d.created) != 0 {
		t.Fatal("a refused request created an instance")
	}
}

func TestCreateWithoutBaseReportsNoBase(t *testing.T) {
	c := newCreateEnv(t)
	res, err := c.env.Create(context.Background(), c.req())
	if err != nil {
		t.Fatal(err)
	}
	if res.Base != "" || res.BaseCommit != "" {
		t.Fatalf("base = %q @ %q, want none", res.Base, res.BaseCommit)
	}
}

func TestCreateDefaultBaseIsOriginHEAD(t *testing.T) {
	c := newCreateEnv(t)
	// Make origin/HEAD point at origin/main, one commit behind local main.
	git(t, c.repo, "update-ref", "refs/remotes/origin/main", "HEAD")
	git(t, c.repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	want := git(t, c.repo, "rev-parse", "HEAD")
	git(t, c.repo, "commit", "-q", "--allow-empty", "-m", "local only")
	if _, err := c.env.Create(context.Background(), c.req()); err != nil {
		t.Fatal(err)
	}
	if got := git(t, filepath.Join(c.wtRoot, "task-1"), "rev-parse", "HEAD"); got != want {
		t.Fatalf("worktree at %s, want origin/HEAD target %s", got, want)
	}
}

func TestCreateValidation(t *testing.T) {
	c := newCreateEnv(t)
	cases := map[string]func(*CreateRequest){
		"bad instance":   func(r *CreateRequest) { r.Instance = "a b" },
		"dot instance":   func(r *CreateRequest) { r.Instance = ".." },
		"bad worktree":   func(r *CreateRequest) { r.Worktree = "../x" },
		"slash worktree": func(r *CreateRequest) { r.Worktree = "a/b" },
		"empty branch":   func(r *CreateRequest) { r.Branch = "" },
		"dash branch":    func(r *CreateRequest) { r.Branch = "-x" },
		"bad ref branch": func(r *CreateRequest) { r.Branch = "a..b" },
		"dash base":      func(r *CreateRequest) { r.Base = "--foo" },
		"bad ref base":   func(r *CreateRequest) { r.Base = "a..b" },
		"relative allow": func(r *CreateRequest) { r.AllowFiles = []string{"rel.txt"} },
		"missing allow":  func(r *CreateRequest) { r.AllowFiles = []string{"/no/such/file"} },
		"other host":     func(r *CreateRequest) { r.Template = "tmpl@elsewhere" },
		"bad template":   func(r *CreateRequest) { r.Template = "nope" },
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			r := c.req()
			mod(&r)
			_, err := c.env.Create(context.Background(), r)
			if name == "other host" {
				wantReason(t, err, safesend.ReasonNotLocal)
			} else {
				wantReason(t, err, safesend.ReasonInvalid)
			}
		})
	}
	if len(c.d.created) != 0 {
		t.Fatal("a refused request created an instance")
	}
}

func TestCreateAllowFiles(t *testing.T) {
	c := newCreateEnv(t)
	note, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(note, "note.md")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := c.req()
	r.AllowFiles = []string{f}
	if _, err := c.env.Create(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if got := c.d.created[0].AllowFiles; len(got) != 1 || got[0] != f {
		t.Fatalf("AllowFiles = %v", got)
	}
}

func TestCreateTemplateErrors(t *testing.T) {
	c := newCreateEnv(t)
	r := c.req()
	r.Template = "missing@" + address.LocalHostName()
	_, err := c.env.Create(context.Background(), r)
	wantReason(t, err, safesend.ReasonNotFound)

	// A template workdir outside any Git checkout.
	plain := t.TempDir()
	envFile := filepath.Join(discovery.EnvDir, "plain.env")
	if err := os.WriteFile(envFile, []byte("AGENTMUX_WORKDIR="+plain+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.Template = "plain@" + address.LocalHostName()
	_, err = c.env.Create(context.Background(), r)
	wantReason(t, err, safesend.ReasonUnsupported)

	// A template that runs as another user.
	cur, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(discovery.EnvDir, "other.env"), []byte("AGENTMUX_WORKDIR="+c.repo+"\nAGENTMUX_RUN_USER="+cur.Username+"x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.Template = "other@" + address.LocalHostName()
	_, err = c.env.Create(context.Background(), r)
	wantReason(t, err, safesend.ReasonUnsupported)
}

// TestCreateCopiesAmpTemplateEnvFile checks the AMUX-14 inheritance: a task
// instance created from an amp template with an op env-file gets its own
// copy (references only, mode 600), so it launches through `op run` like
// its template instead of falling back to the stored `amp login`. The
// template's amp serving knobs (dirs, discover, update, mode) are carried
// over to the new instance too. A template without an env-file leaves no
// file behind.
func TestCreateCopiesAmpTemplateEnvFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	envHome := filepath.Join(home, ".agentmux", "env")
	if err := os.MkdirAll(envHome, 0o755); err != nil {
		t.Fatal(err)
	}
	ref := "AMP_API_KEY=op://vault/item/field\n"
	if err := os.WriteFile(filepath.Join(envHome, "tmpl.env"), []byte(ref), 0o600); err != nil {
		t.Fatal(err)
	}

	c := newCreateEnv(t)
	// A real amp template registry carries no provider fields (the
	// provisioner rejects them for amp), so rewrite the fixture's opencode
	// defaults into a coherent amp template.
	ampReg := "AGENTMUX_AGENT=amp\nAGENTMUX_WORKDIR=" + c.repo + "\n" +
		"AGENTMUX_AMP_DIRS=/srv/a,/srv/b\nAGENTMUX_AMP_DISCOVER_DIRS=1\n" +
		"AGENTMUX_AMP_UPDATE=off\nAGENTMUX_AMP_MODE=high\n"
	if err := os.WriteFile(filepath.Join(discovery.EnvDir, "tmpl.env"), []byte(ampReg), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := c.env.Create(context.Background(), c.req())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created {
		t.Fatal("expected a created instance")
	}
	if len(c.d.created) != 1 {
		t.Fatalf("created = %d instances", len(c.d.created))
	}
	r := c.d.created[0]
	if r.Agent != "amp" || r.AmpDirs != "/srv/a,/srv/b" || !r.AmpDiscoverDirs || r.AmpUpdate != "off" || r.AmpMode != "high" {
		t.Fatalf("amp fields not carried over: %+v", r)
	}
	if r.Provider != "" || r.Model != "" || r.ProviderBaseUrl != "" || r.ProviderApiKeyEnv != "" {
		t.Fatalf("provider fields should stay empty for amp: %+v", r)
	}

	got, err := os.ReadFile(filepath.Join(envHome, "task-1.env"))
	if err != nil {
		t.Fatalf("reading inherited env-file: %v", err)
	}
	if string(got) != ref {
		t.Errorf("inherited env-file = %q, want the template's references %q", got, ref)
	}
	if fi, err := os.Stat(filepath.Join(envHome, "task-1.env")); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("inherited env-file mode = %o, want 600", fi.Mode().Perm())
	}

	// A second create with the same arguments reuses the instance and must
	// not touch the env-file.
	sentinel := "AMP_API_KEY=op://other/ref\n"
	if err := os.WriteFile(filepath.Join(envHome, "task-1.env"), []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err = c.env.Create(context.Background(), c.req())
	if err != nil {
		t.Fatal(err)
	}
	if res.Created {
		t.Error("second create should reuse the instance")
	}
	if got, _ := os.ReadFile(filepath.Join(envHome, "task-1.env")); string(got) != sentinel {
		t.Errorf("reuse overwrote the env-file = %q, want it left alone", got)
	}
}

// TestCreateWithoutTemplateEnvFileLeavesNone checks the other side: an amp
// template with no op env-file creates an instance with no env-file, and a
// non-amp template never gains one either.
func TestCreateWithoutTemplateEnvFileLeavesNone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	c := newCreateEnv(t)
	ampReg := "AGENTMUX_AGENT=amp\nAGENTMUX_WORKDIR=" + c.repo + "\n"
	if err := os.WriteFile(filepath.Join(discovery.EnvDir, "tmpl.env"), []byte(ampReg), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.env.Create(context.Background(), c.req()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".agentmux", "env", "task-1.env")); !os.IsNotExist(err) {
		t.Errorf("amp create without a template env-file left task-1.env behind (err=%v)", err)
	}

	c2 := newCreateEnv(t) // default opencode template, no env-file
	req := c2.req()
	req.Instance, req.Branch = "task-2", "feature/task-2"
	if _, err := c2.env.Create(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".agentmux", "env", "task-2.env")); !os.IsNotExist(err) {
		t.Errorf("non-amp create left task-2.env behind (err=%v)", err)
	}
}

func TestCreateDaemonFailure(t *testing.T) {
	c := newCreateEnv(t)
	c.d.failWith = "boom"
	_, err := c.env.Create(context.Background(), c.req())
	wantReason(t, err, safesend.ReasonFailed)
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error lost the daemon message: %v", err)
	}
}
