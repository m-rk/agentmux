# Completed-turn usage events

Every agentmux-managed Claude Code and Codex instance, task workers and
orchestrators alike, appends one line to the run user's
`~/.agents/local/metrics/turn-completions.jsonl` when a turn finishes. The line
carries the turn's token counts, so a usage ledger (forgentic's usage table)
can record it without reading transcripts, rollouts or run logs itself.
`AGENTMUX_USAGE_EVENTS` overrides the path.

## Payload (v1)

```json
{"v":1,"at":"2026-11-23T12:00:00Z","turn_id":"codex:<thread>:<run>","runner":"codex",
 "instance":"task-mergentic-merg-415","task":"MERG-415","run":"98034db3d313e6f1",
 "project":"m-rk/mergentic","model":"<model>","effort":"medium","session":"<thread>",
 "requests":0,"input_tokens":1200,"output_tokens":340,"cache_read_tokens":88000,
 "cache_write_tokens":0,"reasoning_tokens":120}
```

- `v` stays 1. Later versions only add fields; none is renamed or removed.
- `turn_id` is stable for one turn: `claude:<session>:<last assistant uuid>`
  or `codex:<thread>:<launcher run id>`. Key rows on it.
- `runner` is `claude-code` or `codex`. `session` is the Claude session id or
  the Codex thread id.
- `task`, `run` and `project` come from the `task_id`, `run_id` and `project`
  keys of the instance's stored `OTEL_RESOURCE_ATTRIBUTES` (stamped by the
  dispatcher). Without them `project` is `AGENTMUX_PROJECT` minus
  `github.com/`, and a `task-<project>-<key>-<n>` instance gives `task`
  `<KEY>-<n>`. `task`, `run`, `project`, `model`, `effort` and `session` are
  left out when unknown.
- `input_tokens` is uncached input on both runners (Codex's own count includes
  the cached part; agentmux subtracts it). `cache_read_tokens` and
  `cache_write_tokens` are the cached parts. `output_tokens` includes
  reasoning; `reasoning_tokens` is informational, not to be added again.
- `requests` is the number of model API messages in the turn, 0 when the
  runner doesn't report it (Codex).

Each line is written with one `write()` under an exclusive `flock` on the file,
so a reader never sees two lines interleaved; a reader should still leave a
trailing line without a newline for its next pass. The file is a queue, not a
ledger: agentmux never truncates it, and a consumer keeps its own byte offset.

## Claude Code

`session run` writes a `Stop` hook into the instance's generated settings
(`--settings`; Claude Code merges it with the user's own hooks):
`agentmux usage-hook claude --instance NAME`. The hook reads the Stop input
(`session_id`, `transcript_path`) and sums the `usage` of the assistant
messages appended to the transcript since its previous run, each message id
once (a streamed message repeats its usage on every line; the last wins).
`model` is the last assistant message's model, falling back to the instance's
pin; `effort` is the pin.

A byte cursor per transcript in `~/.agents/local/metrics/turn-cursors/`, held
under a lock, bounds each read to what was appended and makes a second firing
for the same turn write nothing. A transcript with no cursor yet (an instance
started before the hook existed) is read from at most its last 8 MiB and only
its last turn is counted. Subagent transcripts kept in separate files are not
counted.

## Codex

Codex runs only as `codex exec --json`, one process per turn, started by
`sessions run`/`send`. The run launcher notes where the run's segment starts in
the stream log, and once codex exits, before writing its `agentmux.exit` record
(so a continue on the thread waits for it), runs
`agentmux usage-hook codex --instance NAME --run ID --offset N` with the log on
stdin. `turn.completed` carries the thread's cumulative total, so the hook
subtracts the previous `turn.completed` in the same log, found by reading
backwards from the segment start (at most 32 MiB). `model` and `effort` are
the run's `-m` and `model_reasoning_effort`, or the codex config's defaults.
A run that ends without `turn.completed` (failed or killed) reports no usage
and writes no event.

## Failures

A hook never fails or blocks the turn. A problem (unreadable input, missing
transcript) is appended to `turn-completions.errors` beside the events file.
