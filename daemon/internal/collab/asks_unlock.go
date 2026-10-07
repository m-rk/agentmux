package collab

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// UnlockOptions lets `asks tag` / `asks close` unlock a thread that is
// archived and locked (Mark can't reach those in Discord himself). It is
// never for workers: the CLI refuses it in task sessions, and Actor must
// not look like a worker either. See AMUX-50.
type UnlockOptions struct {
	Unlock bool
	// Actor is who asked (instance name or user), logged per unlock.
	Actor string
}

// unlockThread unarchives and unlocks thread, after checking the guard,
// and logs one line. Callers re-archive and re-lock afterwards.
func (c *Client) unlockThread(ctx context.Context, thread Channel, opts UnlockOptions) error {
	if err := c.checkUnlock(ctx, thread, opts.Actor); err != nil {
		return err
	}
	body := map[string]any{"archived": false, "locked": false}
	if err := c.botJSONBody(ctx, http.MethodPatch, "/channels/"+url.PathEscape(thread.ID), body, nil); err != nil {
		return fmt.Errorf("unlocking Discord thread %s (the bot needs Manage Threads on the forum): %w", thread.ID, err)
	}
	c.logUnlock(thread.ID, opts.Actor)
	return nil
}

// checkUnlock allows only threads the bot created in the asks forum
// (askThread already pinned the forum), never the agent test thread and
// never a worker identity.
func (c *Client) checkUnlock(ctx context.Context, thread Channel, actor string) error {
	if strings.HasPrefix(strings.TrimSpace(actor), "task-") {
		return fmt.Errorf("unlock refused: workers can't unlock threads")
	}
	if c.IsTestThread(thread.ID) {
		return fmt.Errorf("unlock refused: %s is the agent test thread", thread.ID)
	}
	var me struct {
		ID string `json:"id"`
	}
	if err := c.botJSON(ctx, http.MethodGet, "/users/@me", &me); err != nil {
		return fmt.Errorf("unlock refused: can't read the bot identity: %w", err)
	}
	if me.ID == "" || thread.OwnerID != me.ID {
		return fmt.Errorf("unlock refused: thread %s wasn't created by the bot", thread.ID)
	}
	return nil
}

// logUnlock appends "- <time> unlock thread <id> by <who>" to the
// configured unlock log (the autopilot log), else the process log.
func (c *Client) logUnlock(threadID, actor string) {
	if actor == "" {
		actor = "unknown"
	}
	line := fmt.Sprintf("- %s asks unlock thread %s by %s", time.Now().UTC().Format("2006-01-02 15:04Z"), threadID, actor)
	if path := strings.TrimSpace(c.Config.UnlockLog); path != "" {
		if f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644); err == nil {
			_, _ = f.WriteString(line + "\n")
			_ = f.Close()
			return
		}
	}
	log.Print(line)
}
