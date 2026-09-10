package collab

import (
	"strings"
	"testing"
)

func TestIdentity(t *testing.T) {
	identity := Identity{Instance: "kilo-minecraft", Host: "build-box.example.net"}
	if got, want := identity.DisplayName(), "kilo-minecraft · build-box.example.net"; got != want {
		t.Fatalf("DisplayName() = %q, want %q", got, want)
	}
	if got, want := identity.Address(), "@kilo-minecraft@build-box.example.net"; got != want {
		t.Fatalf("Address() = %q, want %q", got, want)
	}
}

func TestProjectThreadRouting(t *testing.T) {
	name, err := ProjectThreadName("github.com/m-rk/agentmux", "share runtime finding", false)
	if err != nil {
		t.Fatal(err)
	}
	if name != "[github.com/m-rk/agentmux] share runtime finding" {
		t.Fatalf("unexpected name %q", name)
	}
	if !ThreadRelevant(name, "github.com/m-rk/agentmux") {
		t.Fatal("same-project thread should be relevant")
	}
	if ThreadRelevant(name, "github.com/example/other") {
		t.Fatal("other-project thread should not be relevant")
	}
	shared, err := ProjectThreadName("github.com/m-rk/agentmux", "cross-project note", true)
	if err != nil {
		t.Fatal(err)
	}
	if !ThreadRelevant(shared, "github.com/example/other") {
		t.Fatal("shared thread should be relevant across projects")
	}
}

func TestMessageLimits(t *testing.T) {
	if err := ValidateThreadSummary("A concise elevator pitch."); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"missing punctuation", "two\nlines."} {
		if err := ValidateThreadSummary(invalid); err == nil {
			t.Fatalf("ValidateThreadSummary(%q) unexpectedly succeeded", invalid)
		}
	}
	if err := ValidateInlineReply(strings.Repeat("word ", 201)); err == nil {
		t.Fatal("201-word inline reply unexpectedly succeeded")
	}
	if err := ValidateInlineReply(strings.Repeat("abcdefghijklmnopqrst ", 100)); err == nil {
		t.Fatal("reply beyond Discord's character limit unexpectedly succeeded")
	}
}

func TestAvatarURL(t *testing.T) {
	if err := ValidateAvatarURL("https://example.com/avatar.png"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAvatarURL("http://example.com/avatar.png"); err == nil {
		t.Fatal("insecure avatar URL unexpectedly succeeded")
	}
}

func TestAddressMatchingUsesWholeToken(t *testing.T) {
	if !containsAddress("(@kilo@host), please look", "@kilo@host") {
		t.Fatal("punctuated exact address was not detected")
	}
	if containsAddress("@kilo@hostname please look", "@kilo@host") {
		t.Fatal("address prefix was misclassified as an exact request")
	}
}
