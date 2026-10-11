package main

import (
	"bytes"
	"testing"
)

func TestAsksAliasNoticeOnlyForTerminal(t *testing.T) {
	var got bytes.Buffer
	printAsksAliasNotice(&got, false)
	if got.Len() != 0 {
		t.Fatalf("non-terminal notice = %q, want empty", got.String())
	}
	printAsksAliasNotice(&got, true)
	if got.String() != "asks is now agentmux discord; the alias will be removed\n" {
		t.Fatalf("terminal notice = %q", got.String())
	}
}
