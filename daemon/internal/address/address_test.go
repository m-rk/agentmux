package address

import (
	"errors"
	"testing"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Address
	}{
		{"mergentic@build-box", Address{Instance: "mergentic", Host: "build-box"}},
		{"mergentic-amp@Build-Box#T-01a0-abc", Address{Instance: "mergentic-amp", Host: "build-box", Thread: "T-01a0-abc"}},
		{"rec.tan_gl@h1#9afca08b-e76e", Address{Instance: "rec.tan_gl", Host: "h1", Thread: "9afca08b-e76e"}},
	} {
		got, err := Parse(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("Parse(%q) = %+v, %v; want %+v", tc.in, got, err, tc.want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	for _, in := range []string{
		"",
		"mergentic",           // no host
		"@host",               // no instance
		"mergentic@",          // no host
		"merg entic@host",     // space in instance
		"mergentic@host.lan",  // host must be one label
		"mergentic@-host",     // bad label
		"mergentic@local",     // not fleet-unique
		"mergentic@host#",     // empty thread
		"mergentic@host#a b",  // space in thread
		"mergentic@host#a@b",  // @ in thread
		"mergentic@host@more", // second @ lands in host
	} {
		if a, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) = %+v, want error", in, a)
		}
	}
}

func TestStringRoundTrip(t *testing.T) {
	for _, in := range []string{"a@h", "a@h#T-1"} {
		a, err := Parse(in)
		if err != nil || a.String() != in {
			t.Errorf("round trip %q: got %q, %v", in, a.String(), err)
		}
	}
	a, _ := Parse("a@h#T-1")
	if a.Session().String() != "a@h" {
		t.Errorf("Session() = %q", a.Session())
	}
}

func TestCanonical(t *testing.T) {
	defer func(orig func() (string, error)) { hostname = orig }(hostname)
	hostname = func() (string, error) { return "Build-Box.lan", nil }
	if got := Canonical("local"); got != "build-box" {
		t.Errorf("Canonical(local) = %q", got)
	}
	if got := Canonical("Build-Box"); got != "build-box" {
		t.Errorf("Canonical(Build-Box) = %q", got)
	}
	hostname = func() (string, error) { return "", errors.New("no hostname") }
	if got := LocalHostName(); got != LocalAlias {
		t.Errorf("LocalHostName with error = %q", got)
	}
}
