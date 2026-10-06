package collab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAsksList serves the forum, the guild active threads, and two pages of
// public archived threads.
type fakeAsksList struct {
	mu       sync.Mutex
	open     []Channel
	archived []Channel
	pages    int
}

func (f *fakeAsksList) server(t *testing.T) *httptest.Server {
	forum := Channel{ID: "forum", GuildID: "guild", Type: 15, AvailableTags: []ForumTag{
		{"t-ask", "ask"}, {"t-pending", "pending"}, {"t-answered", "answered"}, {"t-proj", "mergentic"},
	}}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/api/channels/forum":
			writeJSON(t, w, forum)
		case r.URL.Path == "/api/guilds/guild/threads/active":
			writeJSON(t, w, threadList{Threads: f.open})
		case strings.HasSuffix(r.URL.Path, "/threads/archived/public"):
			f.pages++
			if before := r.URL.Query().Get("before"); before == "" {
				writeJSON(t, w, threadList{Threads: f.archived[:1], HasMore: true})
			} else {
				writeJSON(t, w, threadList{Threads: f.archived[1:]})
			}
		default:
			http.NotFound(w, r)
		}
	}))
}

// snowflakeAt builds a Discord snowflake id stamping the given time, so
// tests exercise the timestamp fallback with realistic ids.
func snowflakeAt(t time.Time) string {
	ms := t.UTC().UnixMilli() - 1420070400000
	if ms < 0 {
		ms = 0
	}
	return strconv.FormatUint(uint64(ms)<<22|1, 10)
}

func TestListAsksOpenSkipsNonAsks(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	openID, lastID := snowflakeAt(now.Add(-time.Hour)), snowflakeAt(now)
	f := &fakeAsksList{open: []Channel{
		// An open ask, an open non-ask (skipped), and an ask from another
		// forum (skipped).
		{ID: openID, ParentID: "forum", Name: "open ask", AppliedTags: []string{"t-ask", "t-pending"}, LastMessageID: lastID},
		{ID: snowflakeAt(now.Add(-2 * time.Hour)), ParentID: "forum", Name: "session topic"},
		{ID: snowflakeAt(now.Add(-3 * time.Hour)), ParentID: "other", Name: "stray", AppliedTags: []string{"t-ask"}},
	}}
	s := f.server(t)
	defer s.Close()
	got, err := asksClientFor(s.URL).ListAsks(context.Background(), ListAsksOptions{State: "open"})
	if err != nil || len(got) != 1 || got[0].ID != openID {
		t.Fatalf("got %v %v", got, err)
	}
	th := got[0]
	if th.Title != "open ask" || len(th.Tags) != 2 || th.Tags[0] != "ask" || th.Tags[1] != "pending" {
		t.Fatalf("thread = %#v", th)
	}
	if th.Archived || th.Locked || th.StarterMessage != openID {
		t.Fatalf("thread = %#v", th)
	}
	if th.Created != now.Add(-time.Hour).Format(time.RFC3339) || th.LastMessageTime != now.Format(time.RFC3339) {
		t.Fatalf("thread = %#v", th)
	}
}

func TestListAsksArchivedPaginates(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	oldID, olderID := snowflakeAt(now.Add(-24*time.Hour)), snowflakeAt(now.Add(-48*time.Hour))
	f := &fakeAsksList{archived: []Channel{
		{ID: oldID, ParentID: "forum", Name: "old ask", AppliedTags: []string{"t-ask", "t-answered"},
			ThreadMeta: ThreadMetadata{Archived: true, ArchiveTimestamp: now.Add(-time.Hour).Format(time.RFC3339)}},
		{ID: olderID, ParentID: "forum", Name: "older ask", AppliedTags: []string{"t-ask", "t-pending"},
			ThreadMeta: ThreadMetadata{Archived: true, Locked: true, ArchiveTimestamp: now.Add(-2 * time.Hour).Format(time.RFC3339)}},
	}}
	s := f.server(t)
	defer s.Close()
	got, err := asksClientFor(s.URL).ListAsks(context.Background(), ListAsksOptions{State: "archived"})
	if err != nil || len(got) != 2 || got[0].ID != oldID || got[1].ID != olderID {
		t.Fatalf("got %v %v", got, err)
	}
	if f.pages != 2 {
		t.Fatalf("pages = %d, want 2", f.pages)
	}
	if !got[0].Archived || !got[1].Locked {
		t.Fatalf("threads = %#v", got)
	}
}

func TestListAsksAllDedupesAndFilters(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	openID := snowflakeAt(now.Add(-time.Hour))
	taggedID := snowflakeAt(now.Add(-30 * 24 * time.Hour))
	f := &fakeAsksList{
		open: []Channel{
			{ID: openID, ParentID: "forum", Name: "open ask", AppliedTags: []string{"t-ask", "t-pending"},
				LastMessageID: snowflakeAt(now)},
		},
		archived: []Channel{
			// Same id as the active open ask: listed once, as open.
			{ID: openID, ParentID: "forum", Name: "open ask", AppliedTags: []string{"t-ask", "t-pending"},
				ThreadMeta: ThreadMetadata{ArchiveTimestamp: now.Add(-time.Hour).Format(time.RFC3339)}},
			{ID: taggedID, ParentID: "forum", Name: "merged ask", AppliedTags: []string{"t-ask", "t-proj"},
				ThreadMeta: ThreadMetadata{Archived: true, ArchiveTimestamp: now.Add(-30 * 24 * time.Hour).Format(time.RFC3339)}},
		},
	}
	s := f.server(t)
	defer s.Close()
	c := asksClientFor(s.URL)
	got, err := c.ListAsks(context.Background(), ListAsksOptions{State: "all"})
	if len(got) != 2 || err != nil {
		t.Fatalf("got %v %v", got, err)
	}
	if got[0].ID != openID || got[0].Archived || got[1].ID != taggedID || !got[1].Archived {
		t.Fatalf("got %v", got)
	}
	filtered, err := c.ListAsks(context.Background(), ListAsksOptions{State: "all", Tag: "mergentic"})
	if err != nil || len(filtered) != 1 || filtered[0].ID != taggedID {
		t.Fatalf("filtered = %v %v", filtered, err)
	}
	if _, err := c.ListAsks(context.Background(), ListAsksOptions{State: "all", Tag: "nope"}); err == nil {
		t.Fatal("unknown tag accepted")
	}
	recent, err := c.ListAsks(context.Background(), ListAsksOptions{State: "all", Since: now.Add(-time.Hour).Add(-time.Minute)})
	if err != nil || len(recent) != 1 || recent[0].ID != openID {
		t.Fatalf("recent = %v %v", recent, err)
	}
	if _, err := c.ListAsks(context.Background(), ListAsksOptions{State: "bogus"}); err == nil {
		t.Fatal("bad state accepted")
	}
}

func TestListAsksRetriesOnceOnRateLimit(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	openID := snowflakeAt(now)
	var calls int
	var mu sync.Mutex
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/api/channels/forum":
			writeJSON(t, w, Channel{ID: "forum", GuildID: "guild", AvailableTags: []ForumTag{{"t-ask", "ask"}}})
		case r.URL.Path == "/api/guilds/guild/threads/active":
			calls++
			if calls == 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"retry_after": 0}`))
				return
			}
			writeJSON(t, w, threadList{Threads: []Channel{
				{ID: openID, ParentID: "forum", Name: "ask", AppliedTags: []string{"t-ask"}},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer s.Close()
	got, err := asksClientFor(s.URL).ListAsks(context.Background(), ListAsksOptions{State: "open"})
	if err != nil || len(got) != 1 || got[0].ID != openID || calls != 2 {
		t.Fatalf("got %v %v calls=%d", got, err, calls)
	}
}
