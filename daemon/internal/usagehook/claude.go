package usagehook

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// maxClaudeFirstRead bounds the first read of a transcript the hook has no
// cursor for (an instance that was running before the hook existed): only
// its tail is read, and only its last turn is counted.
const maxClaudeFirstRead = 8 << 20

// CursorDir holds one small cursor file per Claude transcript, relative to
// the run user's home.
const CursorDir = ".agents/local/metrics/turn-cursors"

type claudeLine struct {
	Type    string `json:"type"`
	UUID    string `json:"uuid"`
	IsMeta  bool   `json:"isMeta"`
	Message *struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
		Usage   *struct {
			Input      int64 `json:"input_tokens"`
			CacheWrite int64 `json:"cache_creation_input_tokens"`
			CacheRead  int64 `json:"cache_read_input_tokens"`
			Output     int64 `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// ClaudeTurn is the usage appended to a transcript since the last hook.
type ClaudeTurn struct {
	Tokens   Tokens
	Model    string // the last assistant message's model
	LastUUID string // the last assistant line, for the turn id
}

// ReadClaudeTurn sums the assistant usage written to transcript since the
// previous call for that transcript, and moves the cursor (kept under
// cursorDir) to the end of the last complete line. A message streamed over
// several lines repeats its usage, so each message id counts once, its last
// line winning. Calls for one transcript are serialised by a lock on its
// cursor file, so a hook that fires twice counts nothing the second time.
func ReadClaudeTurn(transcript, cursorDir string) (ClaudeTurn, error) {
	var turn ClaudeTurn
	if err := os.MkdirAll(cursorDir, 0o700); err != nil {
		return turn, err
	}
	sum := sha256.Sum256([]byte(transcript))
	cf, err := os.OpenFile(filepath.Join(cursorDir, "claude-"+hex.EncodeToString(sum[:8])+".json"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return turn, err
	}
	defer cf.Close()
	if err := syscall.Flock(int(cf.Fd()), syscall.LOCK_EX); err != nil {
		return turn, err
	}
	defer syscall.Flock(int(cf.Fd()), syscall.LOCK_UN)
	var cur struct {
		Path   string `json:"path"`
		Offset int64  `json:"offset"`
	}
	have := json.NewDecoder(cf).Decode(&cur) == nil && cur.Path == transcript

	f, err := os.Open(transcript)
	if err != nil {
		return turn, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return turn, err
	}
	size := info.Size()
	start := cur.Offset
	// Without a cursor (or after the transcript was replaced) read only the
	// tail and count only the last turn: everything before the last real
	// prompt is history the hook never saw finish.
	lastTurnOnly := !have || start > size
	if lastTurnOnly {
		start = size - maxClaudeFirstRead
		if start < 0 {
			start = 0
		}
	}
	br := bufio.NewReaderSize(io.NewSectionReader(f, start, size-start), 64<<10)
	pos := start
	skipPartial := lastTurnOnly && start > 0
	type msg struct {
		uuid, model     string
		in, out, cr, cw int64
	}
	var order []string
	byID := map[string]msg{}
	for {
		line, n, rerr := readLine(br)
		if rerr != nil {
			break // a trailing line still being written waits for next time
		}
		if skipPartial {
			skipPartial = false
			pos += n
			continue
		}
		pos += n
		var l claudeLine
		if len(line) == 0 || json.Unmarshal(line, &l) != nil || l.Message == nil {
			continue
		}
		switch l.Type {
		case "user":
			if lastTurnOnly && !l.IsMeta && isPrompt(l.Message.Content) {
				order, byID = nil, map[string]msg{}
			}
		case "assistant":
			u := l.Message.Usage
			if u == nil || l.Message.Model == "<synthetic>" {
				continue
			}
			id := l.Message.ID
			if id == "" {
				id = l.UUID
			}
			if _, seen := byID[id]; !seen {
				order = append(order, id)
			}
			byID[id] = msg{l.UUID, l.Message.Model, u.Input, u.Output, u.CacheRead, u.CacheWrite}
		}
	}
	for _, id := range order {
		m := byID[id]
		turn.Tokens.Requests++
		turn.Tokens.Input += m.in
		turn.Tokens.Output += m.out
		turn.Tokens.CacheRead += m.cr
		turn.Tokens.CacheWrite += m.cw
		turn.Model, turn.LastUUID = m.model, m.uuid
	}
	cur.Path, cur.Offset = transcript, pos
	data, _ := json.Marshal(cur)
	if err := cf.Truncate(0); err != nil {
		return turn, err
	}
	if _, err := cf.WriteAt(data, 0); err != nil {
		return turn, err
	}
	return turn, nil
}

// isPrompt reports a user message the person (or a peer) typed, as opposed
// to a tool result fed back to the model.
func isPrompt(content json.RawMessage) bool {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return true
	}
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return false
	}
	for _, b := range blocks {
		if b.Type == "tool_result" {
			return false
		}
	}
	return len(blocks) > 0
}
