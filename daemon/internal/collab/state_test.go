package collab

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateRoundTripAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	want := State{Onboarded: true, LastSeen: map[string]string{"thread": "456"}}
	if err := SaveState(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Onboarded != want.Onboarded || got.LastSeen["thread"] != "456" {
		t.Fatalf("state = %#v", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %o", info.Mode().Perm())
	}
}
