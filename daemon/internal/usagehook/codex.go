package usagehook

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// maxCodexBaseline bounds how far back from a run's start the reader looks
// for the previous run's total. One run's segment is far smaller.
const maxCodexBaseline = 32 << 20

// maxCodexLine is the longest log line parsed; longer ones (a command's
// output) are skipped, never a usage record.
const maxCodexLine = 1 << 20

type codexUsage struct {
	Input      int64 `json:"input_tokens"`
	Cached     int64 `json:"cached_input_tokens"`
	CacheWrite int64 `json:"cache_write_input_tokens"`
	Output     int64 `json:"output_tokens"`
	Reasoning  int64 `json:"reasoning_output_tokens"`
}

type codexLine struct {
	Type     string      `json:"type"`
	ThreadID string      `json:"thread_id"`
	Usage    *codexUsage `json:"usage"`
}

// CodexRun is one `codex exec` run read back from its stream log.
type CodexRun struct {
	Thread string
	Tokens Tokens
	// Completed is false when the run ended without turn.completed (failed
	// or killed): codex reports no usage for it.
	Completed bool
}

// ReadCodexRun reads the run that starts at byte offset start of the
// agentmux codex stream log r. Codex's turn.completed carries the thread's
// cumulative total, so the run's own usage is that total minus the last
// turn.completed before start (an earlier run on the same thread, in the
// same log). Only this run's segment and, backwards from start, as far as
// that previous total are read.
func ReadCodexRun(r io.ReaderAt, size, start int64) (CodexRun, error) {
	var run CodexRun
	if start < 0 || start > size {
		return run, fmt.Errorf("run offset %d outside log of %d bytes", start, size)
	}
	var total *codexUsage
	br := bufio.NewReaderSize(io.NewSectionReader(r, start, size-start), 64<<10)
	for {
		line, _, err := readLine(br)
		if len(line) > 0 {
			var ev codexLine
			if json.Unmarshal(line, &ev) == nil {
				switch ev.Type {
				case "thread.started":
					run.Thread = ev.ThreadID
				case "turn.completed":
					if ev.Usage != nil {
						total = ev.Usage
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return run, err
		}
	}
	if total == nil {
		return run, nil
	}
	base, err := codexBaseline(r, start)
	if err != nil {
		return run, err
	}
	run.Completed = true
	d := func(now, before int64) int64 {
		if now < before {
			return now // a total that went backwards is a new count
		}
		return now - before
	}
	input := d(total.Input, base.Input)
	cached := d(total.Cached, base.Cached)
	if cached > input {
		cached = input
	}
	run.Tokens = Tokens{
		Input:      input - cached,
		CacheRead:  cached,
		CacheWrite: d(total.CacheWrite, base.CacheWrite),
		Output:     d(total.Output, base.Output),
		Reasoning:  d(total.Reasoning, base.Reasoning),
	}
	return run, nil
}

// codexBaseline is the last turn.completed usage before end, or zero if the
// log has none there. It reads backwards in growing windows; a log with no
// earlier total within maxCodexBaseline is an error rather than a guess.
func codexBaseline(r io.ReaderAt, end int64) (codexUsage, error) {
	marker := []byte(`"type":"turn.completed"`)
	for window := int64(256 << 10); ; window *= 4 {
		if window > maxCodexBaseline {
			window = maxCodexBaseline
		}
		from := end - window
		if from < 0 {
			from = 0
		}
		buf := make([]byte, end-from)
		if _, err := r.ReadAt(buf, from); err != nil && err != io.EOF {
			return codexUsage{}, err
		}
		// Drop a partial first line unless the window starts the file.
		if from > 0 {
			if i := bytes.IndexByte(buf, '\n'); i >= 0 {
				buf = buf[i+1:]
			} else {
				buf = nil
			}
		}
		for len(buf) > 0 {
			i := bytes.LastIndex(buf, marker)
			if i < 0 {
				break
			}
			lineStart := bytes.LastIndexByte(buf[:i], '\n') + 1
			lineEnd := bytes.IndexByte(buf[i:], '\n')
			line := buf[lineStart:]
			if lineEnd >= 0 {
				line = buf[lineStart : i+lineEnd]
			}
			var ev codexLine
			if json.Unmarshal(line, &ev) == nil && ev.Type == "turn.completed" && ev.Usage != nil {
				return *ev.Usage, nil
			}
			buf = buf[:lineStart]
		}
		if from == 0 {
			return codexUsage{}, nil
		}
		if window == maxCodexBaseline {
			return codexUsage{}, fmt.Errorf("no earlier codex total within %d bytes of the run", maxCodexBaseline)
		}
	}
}

// readLine returns the next line without its newline and the bytes it
// took; a line longer than maxCodexLine comes back empty (skipped).
func readLine(br *bufio.Reader) ([]byte, int64, error) {
	var line []byte
	var n int64
	long := false
	for {
		chunk, err := br.ReadSlice('\n')
		n += int64(len(chunk))
		if !long {
			if len(line)+len(chunk) > maxCodexLine {
				long, line = true, nil
			} else {
				line = append(line, chunk...)
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if long {
			line = nil
		}
		return bytes.TrimRight(line, "\r\n"), n, err
	}
}
