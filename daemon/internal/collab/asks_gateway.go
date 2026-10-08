package collab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const defaultGatewayURL = "wss://gateway.discord.gg/?v=10&encoding=json"

// interactionAckBudget is under Discord's 3-second window for responding to
// an interaction.
const interactionAckBudget = 2500 * time.Millisecond

const guildMessagesIntent = 1 << 9

var wakeMu sync.Mutex
var lastWake time.Time

func writeWake(path string) error {
	wakeMu.Lock()
	defer wakeMu.Unlock()
	now := time.Now()
	if now.Sub(lastWake) < 300*time.Millisecond {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chtimes(path, now, now); err != nil {
		return err
	}
	lastWake = now
	return nil
}

// Listener holds a Discord gateway connection open and records clicks on ask
// buttons. Button clicks reach a bot only as INTERACTION_CREATE events, so
// this is what makes the buttons approach need an always-on process. No
// privileged intents are required.
type Listener struct {
	Client     *Client
	Store      *ClickStore
	WakePath   string
	GatewayURL string
	Logf       func(format string, args ...any)

	sessionID string
	resumeURL string
	seq       atomic.Int64
}

type gatewayFrame struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  *int64          `json:"s"`
	T  string          `json:"t"`
}

func (l *Listener) heartbeatSeq() any {
	if s := l.seq.Load(); s > 0 {
		return s
	}
	return nil
}

func (l *Listener) logf(format string, args ...any) {
	if l.Logf != nil {
		l.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// Run connects and serves until ctx is cancelled, reconnecting with backoff.
func (l *Listener) Run(ctx context.Context) error {
	backoff := time.Second
	for {
		ready, err := l.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		l.logf("asks gateway: %v; reconnecting in %s", err, backoff)
		if ready {
			backoff = time.Second
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil
		}
		if backoff < time.Minute {
			backoff *= 2
		}
	}
}

// session runs one connection and reports whether it got as far as READY or
// RESUMED.
func (l *Listener) session(ctx context.Context) (ready bool, err error) {
	target := l.GatewayURL
	if target == "" {
		target = defaultGatewayURL
	}
	if l.sessionID != "" && l.resumeURL != "" {
		target = l.resumeURL
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, target, nil)
	if err != nil {
		return false, fmt.Errorf("dialing gateway: %w", err)
	}
	defer conn.Close()
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	var writeMu sync.Mutex
	send := func(op int, d any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteJSON(map[string]any{"op": op, "d": d})
	}

	var hello gatewayFrame
	if err := conn.ReadJSON(&hello); err != nil || hello.Op != 10 {
		return false, fmt.Errorf("expected HELLO from gateway: %v", err)
	}
	var h struct {
		HeartbeatInterval float64 `json:"heartbeat_interval"`
	}
	if err := json.Unmarshal(hello.D, &h); err != nil || h.HeartbeatInterval <= 0 {
		return false, fmt.Errorf("bad HELLO payload")
	}
	interval := time.Duration(h.HeartbeatInterval * float64(time.Millisecond))

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if send(1, l.heartbeatSeq()) != nil {
					return
				}
			case <-stop:
				return
			}
		}
	}()

	if l.sessionID != "" {
		err = send(6, map[string]any{"token": l.Client.Config.BotToken, "session_id": l.sessionID, "seq": l.seq.Load()})
	} else {
		err = send(2, map[string]any{
			"token":      l.Client.Config.BotToken,
			"intents":    guildMessagesIntent,
			"properties": map[string]string{"os": "linux", "browser": "agentmux", "device": "agentmux"},
		})
	}
	if err != nil {
		return false, err
	}

	for {
		// Discord ACKs every heartbeat, so silence for two intervals means a
		// dead connection.
		_ = conn.SetReadDeadline(time.Now().Add(2*interval + 5*time.Second))
		var f gatewayFrame
		if err := conn.ReadJSON(&f); err != nil {
			return ready, fmt.Errorf("reading gateway: %w", err)
		}
		if f.S != nil {
			l.seq.Store(*f.S)
		}
		switch f.Op {
		case 0:
			switch f.T {
			case "READY":
				var r struct {
					SessionID string `json:"session_id"`
					ResumeURL string `json:"resume_gateway_url"`
				}
				_ = json.Unmarshal(f.D, &r)
				l.sessionID, l.resumeURL = r.SessionID, r.ResumeURL
				if l.resumeURL != "" && !strings.Contains(l.resumeURL, "?") {
					l.resumeURL += "/?v=10&encoding=json"
				}
				ready = true
				l.logf("asks gateway: connected")
			case "RESUMED":
				ready = true
			case "INTERACTION_CREATE":
				raw := f.D
				go func() {
					ictx, cancel := context.WithTimeout(ctx, interactionAckBudget)
					defer cancel()
					if err := l.HandleInteraction(ictx, raw); err != nil {
						l.logf("asks gateway: interaction: %v", err)
					}
				}()
			case "MESSAGE_CREATE":
				raw := f.D
				go func() {
					ictx, cancel := context.WithTimeout(ctx, interactionAckBudget)
					defer cancel()
					if err := l.HandleMessage(ictx, raw); err != nil {
						l.logf("asks gateway: message: %v", err)
					}
				}()
			}
		case 1:
			_ = send(1, l.heartbeatSeq())
		case 7:
			return ready, fmt.Errorf("gateway asked to reconnect")
		case 9:
			var resumable bool
			_ = json.Unmarshal(f.D, &resumable)
			if !resumable {
				l.sessionID, l.resumeURL = "", ""
				l.seq.Store(0)
			}
			return ready, fmt.Errorf("gateway invalidated the session")
		}
	}
}

type interaction struct {
	ID        string `json:"id"`
	Token     string `json:"token"`
	Type      int    `json:"type"`
	ChannelID string `json:"channel_id"`
	Channel   struct {
		ParentID string `json:"parent_id"`
	} `json:"channel"`
	Member *struct {
		User Author `json:"user"`
	} `json:"member"`
	User *Author `json:"user"`
	Data struct {
		CustomID string `json:"custom_id"`
	} `json:"data"`
	Message struct {
		ID         string           `json:"id"`
		Components []map[string]any `json:"components"`
	} `json:"message"`
}

// HandleInteraction processes one INTERACTION_CREATE payload: it records a
// click from the configured user and acks by disabling the buttons with the
// chosen one marked, or answers anyone else with a private refusal. It does
// no Discord API reads, so it fits inside the 3-second ack window.
func (l *Listener) HandleInteraction(ctx context.Context, raw json.RawMessage) error {
	var in interaction
	if err := json.Unmarshal(raw, &in); err != nil {
		return err
	}
	const messageComponent = 3
	if in.Type != messageComponent || !strings.HasPrefix(in.Data.CustomID, buttonIDPrefix) {
		return nil
	}
	user := in.User
	if in.Member != nil {
		user = &in.Member.User
	}
	if user == nil {
		return fmt.Errorf("interaction %s has no user", in.ID)
	}
	if in.Channel.ParentID != l.Client.Config.ForumChannelID {
		return l.respond(ctx, in, 4, map[string]any{"content": "That button isn't on an ask.", "flags": 64})
	}
	configured, err := l.Client.mentionUser()
	if err != nil {
		return err
	}
	if in.ChannelID == strings.TrimSpace(l.Client.Config.TestThreadID) && l.Client.Config.TestThreadID != "" {
		// Clicks on test-thread buttons can never answer a real ask:
		// they are answered ephemerally and recorded nowhere.
		return l.respond(ctx, in, 4, map[string]any{"content": "Test button — this click does nothing.", "flags": 64})
	}
	if user.ID != configured {
		return l.respond(ctx, in, 4, map[string]any{"content": "Only the person this ask is for can answer it.", "flags": 64})
	}
	label := strings.TrimPrefix(in.Data.CustomID, buttonIDPrefix)
	recorded, err := l.Store.Append(Click{
		ThreadID: in.ChannelID, MessageID: in.Message.ID, Label: label, UserID: user.ID, Timestamp: time.Now().UTC(),
	})
	if err != nil {
		_ = l.respond(ctx, in, 4, map[string]any{"content": "Couldn't record that answer; try again.", "flags": 64})
		return fmt.Errorf("recording click: %w", err)
	}
	if recorded {
		l.wake()
	}
	if !recorded {
		return l.respond(ctx, in, 4, map[string]any{"content": "This ask already has an answer.", "flags": 64})
	}
	if err := l.respond(ctx, in, 7, map[string]any{"components": settleComponents(in.Message.Components, in.Data.CustomID)}); err != nil {
		return err
	}
	if err := l.Client.putReaction(ctx, in.ChannelID, in.Message.ID, "👀"); err != nil {
		l.logf("asks gateway: adding received reaction: %v", err)
	}
	return nil
}

type gatewayMessage struct {
	ID        string    `json:"id"`
	ChannelID string    `json:"channel_id"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
	Author    Author    `json:"author"`
}

// HandleMessage acknowledges replies by the configured user in the asks
// forum. Discord remains the source of reply text; this store records only
// metadata so ask serve can wake promptly without requiring MESSAGE_CONTENT.
func (l *Listener) HandleMessage(ctx context.Context, raw json.RawMessage) error {
	var m gatewayMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	if m.Author.Bot || m.Author.ID == "" || m.ID == "" || m.ChannelID == "" {
		return nil
	}
	if m.ChannelID == strings.TrimSpace(l.Client.Config.TestThreadID) && l.Client.Config.TestThreadID != "" {
		return nil
	}
	user, err := l.Client.mentionUser()
	if err != nil {
		return err
	}
	if m.Author.ID != user {
		return nil
	}
	var channel Channel
	if err := l.Client.botJSON(ctx, http.MethodGet, "/channels/"+url.PathEscape(m.ChannelID), &channel); err != nil {
		return err
	}
	if channel.ParentID != l.Client.Config.ForumChannelID {
		return nil
	}
	var forum Channel
	if err := l.Client.botJSON(ctx, http.MethodGet, "/channels/"+url.PathEscape(l.Client.Config.ForumChannelID), &forum); err != nil {
		return err
	}
	if !l.Client.isAskThread(forum, channel) {
		return nil
	}
	if m.Timestamp.IsZero() {
		m.Timestamp = time.Now().UTC()
	}
	added, err := l.Store.AppendInput(Input{ThreadID: m.ChannelID, MessageID: m.ID, Timestamp: m.Timestamp.UTC()})
	if err != nil || !added {
		return err
	}
	if err := l.Client.putReaction(ctx, m.ChannelID, m.ID, "👀"); err != nil {
		return err
	}
	l.wake()
	return nil
}

func (l *Listener) wake() {
	path := l.WakePath
	if path == "" && l.Store != nil {
		path = l.Store.Path + ".wake"
	}
	if path == "" {
		return
	}
	if err := writeWake(path); err != nil {
		l.logf("asks gateway: wake ask serve: %v", err)
	}
}

// settleLabel returns the chosen button's label: any existing check marks
// (leading "✓ " or trailing " ✓", any count) stripped, exactly one " ✓" at
// the end. Settling is idempotent — the gateway click handler and the `asks
// edit -chosen` step may run in either order, and each must leave exactly
// one check.
func settleLabel(label string) string {
	return settleBase(label) + " ✓"
}

// checkMarks are the glyphs treated as a settle check: ✓, ✔, and either with
// the emoji variation selector.
const checkMarks = "\u2713\u2714\ufe0f"

// settleBase strips any existing check marks (leading or trailing, any
// count, any variant in checkMarks) so re-settling a settled label stays at
// exactly one check.
func settleBase(label string) string {
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(label), checkMarks+" \t"))
}

// settleComponents disables every button and marks the chosen one with a
// trailing ✓.
func settleComponents(rows []map[string]any, chosen string) []map[string]any {
	const styleSuccess = 3
	for _, row := range rows {
		buttons, _ := row["components"].([]any)
		for _, b := range buttons {
			btn, ok := b.(map[string]any)
			if !ok {
				continue
			}
			btn["disabled"] = true
			if btn["custom_id"] == chosen {
				btn["style"] = styleSuccess
				if label, ok := btn["label"].(string); ok {
					btn["label"] = settleLabel(label)
				}
			} else if label, ok := btn["label"].(string); ok {
				btn["label"] = settleBase(label)
			}
		}
	}
	return rows
}

func (l *Listener) respond(ctx context.Context, in interaction, kind int, data map[string]any) error {
	body, err := json.Marshal(map[string]any{"type": kind, "data": data})
	if err != nil {
		return err
	}
	base := strings.TrimRight(l.Client.APIBaseURL, "/")
	if base == "" {
		base = discordAPIBaseURL
	}
	endpoint := base + "/interactions/" + url.PathEscape(in.ID) + "/" + url.PathEscape(in.Token) + "/callback"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return l.Client.doJSON(req, nil)
}
