package allowfiles

import "testing"

func TestKiloAllowConfigMatchesOpencode(t *testing.T) {
	paths := []string{"/vault/t/TASK-4 [a] (b).md"}
	k, err := KiloAllowConfig("/wt/p", paths)
	if err != nil {
		t.Fatal(err)
	}
	o, err := OpencodeAllowConfig("/wt/p", paths)
	if err != nil {
		t.Fatal(err)
	}
	if string(k) != string(o) {
		t.Fatalf("kilo %s != opencode %s", k, o)
	}
	if _, err := KiloAllowConfig("/wt/p", []string{"/vault/a*.md"}); err == nil {
		t.Fatal("glob character accepted")
	}
	if KiloConfigEnv != "KILO_CONFIG_CONTENT" || OpencodeConfigEnv != "OPENCODE_CONFIG_CONTENT" {
		t.Fatal("env names changed")
	}
}
