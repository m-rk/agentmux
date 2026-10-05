package allowfiles

import "testing"

func TestAmpAllowArgsEmpty(t *testing.T) {
	if got := AmpAllowArgs([]string{"/vault/TASK-4 [a].md"}); len(got) != 0 {
		t.Fatalf("got %q, want no args", got)
	}
}
