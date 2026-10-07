package collab

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func lockedThread(owner string) map[string]Channel {
	return map[string]Channel{
		"900": {ID: "900", ParentID: "forum", OwnerID: owner, AppliedTags: []string{"t-task", "t-done"},
			ThreadMeta: ThreadMetadata{Archived: true, Locked: true}},
	}
}

func TestTagAskUnlockCyclesAndLogs(t *testing.T) {
	s := &tagServer{forum: fullForum(), threads: lockedThread("bot1")}
	srv := s.server(t)
	defer srv.Close()
	c := asksClientFor(srv.URL)
	c.Config.UnlockLog = filepath.Join(t.TempDir(), "autopilot.md")
	if err := c.TagAsk(context.Background(), "900", []string{"task", "working"}, TagOptions{Unlock: true, Actor: "console"}); err != nil {
		t.Fatal(err)
	}
	if len(s.patches) != 4 {
		t.Fatalf("patches = %v, want unlock, unarchive, retag, relock", s.patches)
	}
	if s.patches[0]["locked"] != false || s.patches[0]["archived"] != false {
		t.Fatalf("first patch = %v, want unlock", s.patches[0])
	}
	last := s.patches[3]
	if last["archived"] != true || last["locked"] != true {
		t.Fatalf("last patch = %v, want archived and locked again", last)
	}
	data, err := os.ReadFile(c.Config.UnlockLog)
	if err != nil || !strings.Contains(string(data), "unlock thread 900 by console") || strings.Count(string(data), "\n") != 1 {
		t.Fatalf("log = %q, %v", data, err)
	}
}

func TestTagAskUnlockRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		owner, actor, want string
		test               bool
	}{
		"worker identity": {"bot1", "task-AMUX-1", "workers", false},
		"person's thread": {"person9", "console", "wasn't created by the bot", false},
		"test thread":     {"bot1", "console", "test thread", true},
	} {
		t.Run(name, func(t *testing.T) {
			s := &tagServer{forum: fullForum(), threads: lockedThread(tc.owner)}
			srv := s.server(t)
			defer srv.Close()
			c := asksClientFor(srv.URL)
			if tc.test {
				c.Config.TestThreadID = "900"
			}
			err := c.TagAsk(context.Background(), "900", []string{"task", "working"}, TagOptions{Unlock: true, Actor: tc.actor})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if len(s.patches) != 0 {
				t.Fatalf("patched despite refusal: %v", s.patches)
			}
		})
	}
}

func TestCloseAskUnlock(t *testing.T) {
	s := &tagServer{forum: fullForum(), threads: lockedThread("bot1")}
	srv := s.server(t)
	defer srv.Close()
	err := asksClientFor(srv.URL).CloseAskWith(context.Background(), "900", "done", true, UnlockOptions{Unlock: true, Actor: "console"})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.patches) != 2 || s.patches[0]["locked"] != false || s.patches[1]["locked"] != true || s.patches[1]["archived"] != true {
		t.Fatalf("patches = %v", s.patches)
	}
}

func TestCloseAskUnlockRefusesPersonThread(t *testing.T) {
	s := &tagServer{forum: fullForum(), threads: lockedThread("person9")}
	srv := s.server(t)
	defer srv.Close()
	err := asksClientFor(srv.URL).CloseAskWith(context.Background(), "900", "done", false, UnlockOptions{Unlock: true, Actor: "console"})
	if err == nil || len(s.patches) != 0 {
		t.Fatalf("err = %v patches = %v", err, s.patches)
	}
}
