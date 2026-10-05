package allowfiles

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func write(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// real resolves t.TempDir symlinks (macOS /var -> /private/var) so
// expectations match what Validate returns.
func real(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestValidate(t *testing.T) {
	root := real(t, t.TempDir())
	workdir := filepath.Join(root, "work")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	note := write(t, filepath.Join(root, "vault", "TASK-4 Some title.md"))
	inWork := write(t, filepath.Join(workdir, "a.md"))
	link := filepath.Join(root, "link.md")
	if err := os.Symlink(note, link); err != nil {
		t.Fatal(err)
	}
	workLink := filepath.Join(root, "worklink.md")
	if err := os.Symlink(inWork, workLink); err != nil {
		t.Fatal(err)
	}
	q := write(t, filepath.Join(root, "vault", "what?.md"))
	brace := write(t, filepath.Join(root, "vault", "{a,b}.md"))
	bs := write(t, filepath.Join(root, "vault", `back\slash.md`))
	special := write(t, filepath.Join(root, "vault", "a [x] (y)*.md"))
	var many []string
	for i := 0; i <= MaxFiles; i++ {
		many = append(many, note)
	}

	cases := []struct {
		name    string
		in      []string
		want    []string
		wantErr string
	}{
		{"plain", []string{note}, []string{note}, ""},
		{"symlink resolved to real target", []string{link}, []string{note}, ""},
		{"duplicates collapse", []string{note, link, note}, []string{note}, ""},
		{"special chars ok (escaped later)", []string{special}, []string{special}, ""},
		{"none", nil, nil, ""},
		{"relative", []string{"vault/x.md"}, nil, "absolute"},
		{"not clean", []string{root + "/vault/../vault/TASK-4 Some title.md"}, nil, "clean"},
		{"trailing slash", []string{note + "/"}, nil, "clean"},
		{"missing", []string{filepath.Join(root, "vault", "gone.md")}, nil, "no such file"},
		{"directory", []string{filepath.Join(root, "vault")}, nil, "not a regular file"},
		{"inside workdir", []string{inWork}, nil, "inside the workdir"},
		{"symlink into workdir", []string{workLink}, nil, "inside the workdir"},
		{"question mark", []string{q}, nil, "can't be granted"},
		{"braces", []string{brace}, nil, "can't be granted"},
		{"backslash", []string{bs}, nil, "can't be granted"},
		{"too many", many, nil, "too many"},
		{"empty", []string{""}, nil, "empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Validate(tc.in, workdir)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Validate = %v, %v; want error containing %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Validate = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEncodeDecode(t *testing.T) {
	if got := Encode(nil); got != "" {
		t.Errorf("Encode(nil) = %q, want empty", got)
	}
	in := []string{"/v/a b.md", "/v/c [x].md"}
	enc := Encode(in)
	if strings.ContainsAny(enc, "\n\r") {
		t.Errorf("Encode produced a multi-line value: %q", enc)
	}
	out, err := Decode(enc)
	if err != nil || !reflect.DeepEqual(out, in) {
		t.Errorf("Decode(Encode(x)) = %v, %v; want %v", out, err, in)
	}
	if out, err := Decode(""); out != nil || err != nil {
		t.Errorf("Decode(\"\") = %v, %v", out, err)
	}
	if _, err := Decode("not json"); err == nil {
		t.Error("Decode(garbage) = nil error")
	}
}

func TestExistingDropsMissing(t *testing.T) {
	root := real(t, t.TempDir())
	keep := write(t, filepath.Join(root, "a.md"))
	gone := filepath.Join(root, "gone.md")
	kept, warns := Existing([]string{keep, gone})
	if !reflect.DeepEqual(kept, []string{keep}) || len(warns) != 1 {
		t.Errorf("Existing = %v, %v; want [%s] and one warning", kept, warns, keep)
	}
}
