package allowfiles

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func decodeOrdered(t *testing.T, raw json.RawMessage) ([]string, map[string]string) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not an object: %s", raw)
	}
	var keys []string
	vals := map[string]string{}
	for dec.More() {
		k, _ := dec.Token()
		v, _ := dec.Token()
		keys = append(keys, k.(string))
		vals[k.(string)] = v.(string)
	}
	return keys, vals
}

func TestOpencodeAllowConfig(t *testing.T) {
	note := "/vault/projects/org/proj/tasks/PP-4 Some [title] (x).md"
	got, err := OpencodeAllowConfig("/work/org/proj-wt", []string{note})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"permission":{"external_directory":{"/vault/projects/org/proj/tasks/*":"allow"},` +
		`"read":{"../../../vault/projects/org/proj/tasks/*":"deny","../../../vault/projects/org/proj/tasks/PP-4 Some [title] (x).md":"allow"},` +
		`"edit":{"../../../vault/projects/org/proj/tasks/*":"deny","../../../vault/projects/org/proj/tasks/PP-4 Some [title] (x).md":"allow"}}}`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestOpencodeAllowConfigOrdering(t *testing.T) {
	// A file name sorting before "*" must still come after the deny rule,
	// and nested directories must have all denies ahead of all allows.
	paths := []string{"/v/a/ first.md", "/v/a/b/second.md", "/v/!bang.md"}
	got, err := OpencodeAllowConfig("/w", paths)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Permission struct {
			Read json.RawMessage `json:"read"`
		} `json:"permission"`
	}
	if err := json.Unmarshal(got, &doc); err != nil {
		t.Fatal(err)
	}
	keys, vals := decodeOrdered(t, doc.Permission.Read)
	want := []string{"../v/a/*", "../v/a/b/*", "../v/*", "../v/a/ first.md", "../v/a/b/second.md", "../v/!bang.md"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("order = %q, want %q", keys, want)
	}
	for _, k := range keys[:3] {
		if vals[k] != "deny" {
			t.Fatalf("%s = %s", k, vals[k])
		}
	}
}

func TestOpencodeAllowConfigSkipsInsideAndRejectsBad(t *testing.T) {
	got, err := OpencodeAllowConfig("/w", []string{"/w/in/side.md"})
	if err != nil || string(got) != "{}" {
		t.Fatalf("inside workdir: %s, %v", got, err)
	}
	for _, p := range []string{"/v/a*.md", "/v/a?.md", "/v/*/x.md", "/v/{env:HOME}.md", "/v/{file:x}.md", "rel.md"} {
		if _, err := OpencodeAllowConfig("/w", []string{p}); err == nil {
			t.Errorf("%q accepted", p)
		}
	}
	if _, err := OpencodeAllowConfig("w", []string{"/v/a.md"}); err == nil {
		t.Error("relative workdir accepted")
	}
}

func TestOpencodeAllowConfigQuotesJSON(t *testing.T) {
	got, err := OpencodeAllowConfig("/w", []string{`/v/say "hi" \ <b>&.md`})
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(got) {
		t.Fatalf("invalid JSON: %s", got)
	}
	var doc struct {
		Permission struct {
			Read map[string]string `json:"read"`
		} `json:"permission"`
	}
	if err := json.Unmarshal(got, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Permission.Read[`../v/say "hi" \ <b>&.md`] != "allow" {
		t.Fatalf("round trip lost the name: %s", got)
	}
}
