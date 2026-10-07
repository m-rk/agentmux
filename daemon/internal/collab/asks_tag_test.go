package collab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// tagServer records every thread PATCH (retag may patch up to three times:
// unarchive, retag, re-archive) and serves a forum whose tags can grow
// mid-test, so the refresh-on-unknown-name retry is exercisable.
type tagServer struct {
	mu      sync.Mutex
	patches []map[string]any
	forum   Channel
	threads map[string]Channel
}

func fullForum() Channel {
	return Channel{ID: "forum", GuildID: "guild", Type: 15, AvailableTags: []ForumTag{
		{"t-task", "task"}, {"t-epic", "epic"}, {"t-idea", "idea"}, {"t-spike", "spike"},
		{"t-needsme", "needs me"}, {"t-working", "working"}, {"t-blocked", "blocked"},
		{"t-parked", "parked"}, {"t-notnow", "not now"}, {"t-done", "done"}, {"t-failed", "failed"},
		{"t-ask", "ask"}, {"t-pending", "pending"}, {"t-answered", "answered"}, {"t-launched", "launched"},
		{"t-proj", "mergentic"},
	}}
}

func (s *tagServer) server(t *testing.T) *httptest.Server {
	if s.threads == nil {
		s.threads = map[string]Channel{
			"900": {ID: "900", ParentID: "forum", AppliedTags: []string{"t-task", "t-working", "t-proj"}},
		}
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case r.URL.Path == "/api/users/@me":
			writeJSON(t, w, map[string]string{"id": "bot1"})
		case r.URL.Path == "/api/channels/forum":
			writeJSON(t, w, s.forum)
		case strings.HasPrefix(r.URL.Path, "/api/channels/") && r.Method == http.MethodPatch:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.patches = append(s.patches, body)
			w.WriteHeader(200)
			_, _ = w.Write([]byte("{}"))
		case strings.HasPrefix(r.URL.Path, "/api/channels/"):
			id := strings.TrimPrefix(r.URL.Path, "/api/channels/")
			if th, ok := s.threads[id]; ok {
				writeJSON(t, w, th)
				return
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
}

func appliedIDs(t *testing.T, patch map[string]any) []string {
	t.Helper()
	raw, ok := patch["applied_tags"].([]any)
	if !ok {
		t.Fatalf("applied_tags = %#v", patch["applied_tags"])
	}
	ids := make([]string, 0, len(raw))
	for _, v := range raw {
		ids = append(ids, v.(string))
	}
	return ids
}

func TestTagAskSetsExactly(t *testing.T) {
	s := &tagServer{forum: fullForum()}
	srv := s.server(t)
	defer srv.Close()
	if err := asksClientFor(srv.URL).TagAsk(context.Background(), "900", []string{"task", "working"}, TagOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(s.patches) != 1 {
		t.Fatalf("patches = %d, want 1", len(s.patches))
	}
	if got := appliedIDs(t, s.patches[0]); len(got) != 2 || got[0] != "t-task" || got[1] != "t-working" {
		t.Fatalf("tags = %#v", got)
	}
	if s.patches[0]["archived"] != false {
		t.Fatalf("open thread archived: %#v", s.patches[0])
	}
}

func TestTagAskUnknownNameListsValid(t *testing.T) {
	s := &tagServer{forum: fullForum()}
	srv := s.server(t)
	defer srv.Close()
	err := asksClientFor(srv.URL).TagAsk(context.Background(), "900", []string{"task", "bogus"}, TagOptions{})
	if err == nil {
		t.Fatal("unknown tag accepted")
	}
	if !strings.Contains(err.Error(), `"bogus"`) || !strings.Contains(err.Error(), "needs me") {
		t.Fatalf("err = %v, want the name and the valid list", err)
	}
	if len(s.patches) != 0 {
		t.Fatalf("patches = %d, want none", len(s.patches))
	}
}

func TestTagAskRefreshesForumOnUnknownName(t *testing.T) {
	// First fetch lacks "blocked"; Mark creates it, the retry sees it.
	stale := fullForum()
	stale.AvailableTags = stale.AvailableTags[: len(stale.AvailableTags)-1 : len(stale.AvailableTags)-1]
	s := &tagServer{forum: stale}
	srv := s.server(t)
	defer srv.Close()
	c := asksClientFor(srv.URL)
	// Warm the stale read, then add the tag before the retag's refresh.
	if _, _, err := c.askThread(context.Background(), "900"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.forum = fullForum()
	s.mu.Unlock()
	if err := c.TagAsk(context.Background(), "900", []string{"task", "blocked"}, TagOptions{}); err != nil {
		t.Fatalf("retry after refresh: %v", err)
	}
	if got := appliedIDs(t, s.patches[len(s.patches)-1]); len(got) != 2 || got[1] != "t-blocked" {
		t.Fatalf("tags = %#v", got)
	}
}

func TestTagAskRefusesNonAskThread(t *testing.T) {
	s := &tagServer{forum: fullForum(), threads: map[string]Channel{
		"901": {ID: "901", ParentID: "forum"},
		"902": {ID: "902", ParentID: "other", AppliedTags: []string{"t-task"}},
	}}
	srv := s.server(t)
	defer srv.Close()
	c := asksClientFor(srv.URL)
	if err := c.TagAsk(context.Background(), "901", []string{"task"}, TagOptions{}); err == nil {
		t.Fatal("untagged thread retagged")
	}
	if err := c.TagAsk(context.Background(), "902", []string{"task"}, TagOptions{}); err == nil {
		t.Fatal("other-forum thread retagged")
	}
	if len(s.patches) != 0 {
		t.Fatalf("patches = %d, want none", len(s.patches))
	}
}

func TestTagAskCapsAtFive(t *testing.T) {
	s := &tagServer{forum: fullForum()}
	srv := s.server(t)
	defer srv.Close()
	err := asksClientFor(srv.URL).TagAsk(context.Background(), "900",
		[]string{"task", "working", "blocked", "parked", "not now", "done"}, TagOptions{})
	if err == nil || !strings.Contains(err.Error(), "at most 5 tags") {
		t.Fatalf("err = %v, want the five-tag cap", err)
	}
}

func TestTagAskArchivedNeedsUnarchive(t *testing.T) {
	s := &tagServer{forum: fullForum(), threads: map[string]Channel{
		"900": {ID: "900", ParentID: "forum", AppliedTags: []string{"t-task", "t-working"},
			ThreadMeta: ThreadMetadata{Archived: true}},
	}}
	srv := s.server(t)
	defer srv.Close()
	err := asksClientFor(srv.URL).TagAsk(context.Background(), "900", []string{"task", "done"}, TagOptions{})
	if err == nil || !strings.Contains(err.Error(), "-unarchive") {
		t.Fatalf("err = %v, want the -unarchive hint", err)
	}
	if len(s.patches) != 0 {
		t.Fatalf("patches = %d, want none", len(s.patches))
	}
}

func TestTagAskUnarchiveCyclesArchive(t *testing.T) {
	s := &tagServer{forum: fullForum(), threads: map[string]Channel{
		"900": {ID: "900", ParentID: "forum", AppliedTags: []string{"t-task", "t-working"},
			ThreadMeta: ThreadMetadata{Archived: true}},
	}}
	srv := s.server(t)
	defer srv.Close()
	if err := asksClientFor(srv.URL).TagAsk(context.Background(), "900",
		[]string{"task", "done"}, TagOptions{Unarchive: true}); err != nil {
		t.Fatal(err)
	}
	if len(s.patches) != 3 {
		t.Fatalf("patches = %d, want unarchive+retag+re-archive", len(s.patches))
	}
	if s.patches[0]["archived"] != false || s.patches[2]["archived"] != true {
		t.Fatalf("archive cycle = %#v", s.patches)
	}
	if got := appliedIDs(t, s.patches[1]); len(got) != 2 || got[1] != "t-done" {
		t.Fatalf("tags = %#v", got)
	}
}

func TestTagAskUnarchiveRefusesLocked(t *testing.T) {
	s := &tagServer{forum: fullForum(), threads: map[string]Channel{
		"900": {ID: "900", ParentID: "forum", AppliedTags: []string{"t-task", "t-done"},
			ThreadMeta: ThreadMetadata{Archived: true, Locked: true}},
	}}
	srv := s.server(t)
	defer srv.Close()
	err := asksClientFor(srv.URL).TagAsk(context.Background(), "900",
		[]string{"task", "working"}, TagOptions{Unarchive: true})
	if err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("err = %v, want the locked refusal", err)
	}
}

func TestCloseAskAcceptsAnyStateTag(t *testing.T) {
	for _, state := range []string{"needs me", "blocked", "parked", "not now", "done"} {
		s := &tagServer{forum: fullForum(), threads: map[string]Channel{
			"900": {ID: "900", ParentID: "forum", AppliedTags: []string{"t-task", "t-working"}},
		}}
		srv := s.server(t)
		if err := asksClientFor(srv.URL).CloseAsk(context.Background(), "900", state, false); err != nil {
			srv.Close()
			t.Fatalf("close %q: %v", state, err)
		}
		srv.Close()
	}
}

func TestListAsksFindsTypeTaggedThreads(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/channels/forum":
			writeJSON(t, w, fullForum())
		case r.URL.Path == "/api/guilds/guild/threads/active":
			writeJSON(t, w, threadList{Threads: []Channel{
				{ID: "800000000000000001", ParentID: "forum", Name: "AMUX-35 thread",
					AppliedTags: []string{"t-task", "t-working"}},
				{ID: "800000000000000002", ParentID: "forum", Name: "session topic"},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer s.Close()
	got, err := asksClientFor(s.URL).ListAsks(context.Background(), ListAsksOptions{State: "open"})
	if err != nil || len(got) != 1 || got[0].ID != "800000000000000001" {
		t.Fatalf("got %v %v", got, err)
	}
	if len(got[0].Tags) != 2 || got[0].Tags[0] != "task" || got[0].Tags[1] != "working" {
		t.Fatalf("tags = %#v", got[0].Tags)
	}
}

func TestTagAskSpikeThreadIsAskPost(t *testing.T) {
	// The AMUX-52 retag: a spike-tagged thread must keep working as an ask
	// post, so follow-up post/reply calls don't fail the gate.
	s := &tagServer{forum: fullForum(), threads: map[string]Channel{
		"900": {ID: "900", ParentID: "forum", AppliedTags: []string{"t-spike", "t-working"}},
	}}
	srv := s.server(t)
	defer srv.Close()
	if err := asksClientFor(srv.URL).TagAsk(context.Background(), "900", []string{"spike", "working"}, TagOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := appliedIDs(t, s.patches[0]); len(got) != 2 || got[0] != "t-spike" {
		t.Fatalf("tags = %#v", got)
	}
}

func TestTagAskRefusesSetWithoutKindTag(t *testing.T) {
	s := &tagServer{forum: fullForum()}
	srv := s.server(t)
	defer srv.Close()
	err := asksClientFor(srv.URL).TagAsk(context.Background(), "900", []string{"working"}, TagOptions{})
	if err == nil || !strings.Contains(err.Error(), "keeps one kind tag") {
		t.Fatalf("err = %v, want the kind-tag refusal", err)
	}
	if len(s.patches) != 0 {
		t.Fatalf("patches = %d, want none", len(s.patches))
	}
}

func TestTagAskForceRecoversKindlessThread(t *testing.T) {
	// A thread that already lost its kind tag fails the ask-post gate, so
	// the plain retag can't fix it — -force skips the gate but still
	// requires the new set to carry a kind tag.
	s := &tagServer{forum: fullForum(), threads: map[string]Channel{
		"900": {ID: "900", ParentID: "forum", AppliedTags: []string{"t-working"}},
	}}
	srv := s.server(t)
	defer srv.Close()
	c := asksClientFor(srv.URL)
	if err := c.TagAsk(context.Background(), "900", []string{"spike", "working"}, TagOptions{}); err == nil {
		t.Fatal("plain retag of a kindless thread accepted")
	}
	if err := c.TagAsk(context.Background(), "900", []string{"working"}, TagOptions{Force: true}); err == nil {
		t.Fatal("force retag without a kind tag accepted")
	}
	if err := c.TagAsk(context.Background(), "900", []string{"spike", "working"}, TagOptions{Force: true}); err != nil {
		t.Fatalf("force retag: %v", err)
	}
	if got := appliedIDs(t, s.patches[len(s.patches)-1]); len(got) != 2 || got[0] != "t-spike" {
		t.Fatalf("tags = %#v", got)
	}
}

func TestTagAskKindTagsFromConfig(t *testing.T) {
	// kind_tags in discord.yaml overrides the default kind set: with only
	// "spike" configured, task is no longer a kind tag.
	s := &tagServer{forum: fullForum(), threads: map[string]Channel{
		"900": {ID: "900", ParentID: "forum", AppliedTags: []string{"t-spike", "t-working"}},
	}}
	srv := s.server(t)
	defer srv.Close()
	c := asksClientFor(srv.URL)
	c.Config.KindTags = []string{"spike"}
	err := c.TagAsk(context.Background(), "900", []string{"task", "working"}, TagOptions{})
	if err == nil || !strings.Contains(err.Error(), "keeps one kind tag") {
		t.Fatalf("err = %v, want the kind-tag refusal under the override", err)
	}
	if len(s.patches) != 0 {
		t.Fatalf("patches = %d, want none", len(s.patches))
	}
	if err := c.TagAsk(context.Background(), "900", []string{"spike", "working"}, TagOptions{}); err != nil {
		t.Fatalf("spike retag under the override: %v", err)
	}
}
