# Gateway for orchestrators

Status: proposal. Nothing here is implemented.

An orchestrator agent (a person's delegate, running on one host) needs to see
and talk to agent sessions on every host. This page works out what agentmux
must provide. The consumer is the task-orchestration design in the mergentic
repo (`docs/task-orchestration.md`, "Orchestrator" and phases 3 and 5). That
design assigns the per-host gateway to agentmux and keeps mergentic
independent, so agentmux defines the contract and mergentic integrates
against it.

## What the orchestrator needs

Four operations per host, plus addressing and an event feed:

1. **List** sessions on a host.
2. **Read** a session's recent transcript.
3. **Send** a message to a session.
4. **Status** of a session, richer than running/idle.
5. **Address** a session across hosts: `<instance>@<host>`.
6. **Events**, so a dispatcher or queue service learns that a session needs a
   person without polling.

Constraints from the consumer: reachable across hosts over the tailnet only;
the orchestrator is its own principal, not the person; transcript content is
untrusted data; every send is logged with its source; a fixed-command SSH
fallback exists for when the gateway is down.

## What agentmux already has

- `agentmuxd` gRPC over a Unix socket, optionally over a tailnet TCP address.
  `ListInstances`, `ViewPane`, `SendKeys`, `StreamEvents`, `Control`,
  `CreateInstance`, `RenameInstance`.
- A multi-host client: `hosts.yaml` maps host names to daemon addresses.
- Thread watch (`internal/threadwatch`): per-agent collectors that read the
  runtimes' own records (Claude JSONL, opencode SQLite, amp runner log) into
  typed events with offsets, plus excerpt redaction and awaiting-user
  detection.
- Per-instance tmux servers, so each session is separately addressable.

## Why the existing daemon port is not the gateway

The daemon port has no authentication; access control is the tailnet ACL
(`daemon-tui.md`, "Transport & auth"). That is acceptable for the person's own
devices. It is wrong for an orchestrator, because the same port carries
`Control` (stop, restart), `CreateInstance`, and `SendKeys` (raw keystrokes
into any session, including approval prompts). Handing the orchestrator host
access to that port gives it far more than the four operations.

So the gateway is a separate, narrow listener on its own port. A separate port
also lets Tailscale ACLs allow the orchestrator host to reach the gateway and
not the daemon.

## Gaps

### 1. Transcript read does not exist

`ViewPane` returns the visible pane, which is lossy: wrapped lines, redrawn
TUI chrome, no history. Thread watch reads the real records but emits events
and capped excerpts, not messages.

Needed: a transcript reader per runtime that returns structured messages
(role, time, text, tool calls summarized), paginated by cursor, capped in size,
and redacted.

| Runtime | Source | Notes |
| --- | --- | --- |
| claude-code | `~/.claude/projects/<slug>/*.jsonl` | Reuse the collector's file discovery. |
| opencode | `~/.local/share/opencode/opencode.db` | Read-only SQLite, `message` and `part` tables. |
| amp | none on the host | Thread content is server-side; the runner log only has liveness and errors. Report `transcript: unsupported` and fall back to `ViewPane`. Reading amp threads would need the Amp API; open question. |

Redaction matters more here than in thread watch, because transcripts leave
the host. Reuse the excerpt redactor and add a test corpus of secret shapes.

### 2. Send is raw keystrokes

`SendKeys` is `tmux send-keys` verbatim. A message needs agent-specific
delivery: insert the text literally, submit it, and do it only when the session
can take input. Required behaviour:

- Refuse, with a reason, when the session is dead, mid-turn (unless the caller
  sets `queue`), or showing a modal prompt that would swallow the text.
- Prefix every message with provenance, for example
  `[relayed by orchestrator from <principal>]`, so the receiving agent can tell
  it from the person's typing.
- Return an acknowledgement that the text was submitted, not that the agent
  acted on it. Confirm by observing the transcript or pane change.
- amp runners take their input from threads on ampcode.com, not from the
  tmux pane, so `send` is unsupported for amp until there is an API path.

### 3. Status is too coarse

`Instance.Status` is running, idle, or dead. The orchestrator and the queue
service need: turn in progress, awaiting user (and what kind: question,
permission, plan), last turn error kind, usage-limit, auth failure, last
activity. Thread watch already derives these; expose them per session from its
store instead of recomputing.

### 4. No principal, allowlist, rate limit, or audit

The gateway needs, for sends:

- A caller identity. Preferred: the tailnet peer identity from the local
  Tailscale API (`whois`), which needs no shared secret. Fallback: a bearer
  token resolved through `op run`, never stored in config.
- A config allowlist of principal to instances it may send to. Default deny.
- A per-principal rate limit.
- An append-only audit log (JSON lines): time, principal, target, message
  length and hash, outcome. Message text is optional in the log and off by
  default.

Reads are scoped by the same allowlist. The orchestrator cannot approve
anything on the person's behalf; the gateway has no operation for that, and
approval prompts in a pane are not auto-answered (a `send` into a session
showing a permission prompt is refused, see gap 2).

### 5. Addressing and name registry

Instance names are unique per host (they are registry file names). Across
hosts, uniqueness comes from the pair. Define:

```text
<instance>@<host>             a session
<instance>@<host>#<thread>    a thread inside it (Claude session id, amp
                              threadId, opencode session id)
```

`<host>` is the `hosts.yaml` name, so host names must be unique across the
whole fleet. Needed: a resolver (`agentmux resolve <address>`, and the same in
the gateway) returning host, instance, agent, workdir, and current thread; and
a check that fails loudly when two hosts claim one name. Rename already exists
(`RenameInstance`). The mergentic open question "where does the registry live"
is answered here: each host is authoritative for its own instance names, and
the fleet view is `hosts.yaml` plus a fan-out query. No central store.

paseo names sessions separately. Whether paseo exposes thread read/write the
gateway could reuse is unanswered; check before building duplicate paths.

### 6. Events

`StreamEvents` carries instance status changes only. The queue service and
dispatcher need turn-end, awaiting-user, error and limit events. Expose thread
watch's event stream through the gateway as a filtered server stream, with a
cursor so a reconnecting consumer resumes without loss.

### 7. Platform coverage

Thread watch is Linux-only and runs as the operator user rather than inside
the root `agentmuxd`, so it can read that user's transcripts. On macOS
`agentmuxd` already runs as the user, and macOS hosts are in the fleet. The
gateway's reader must work on both: on Linux as a per-user service like thread
watch, on macOS in-process or as a LaunchAgent. Without this the orchestrator
cannot read half the fleet.

### 8. MCP and SSH fallback

- **MCP.** The orchestrator consumes tools. Provide `agentmux mcp`: a stdio MCP
  server that reads `hosts.yaml`, fans out to each host's gateway, and exposes
  `list_sessions`, `read_session`, `send_message`, `session_status`,
  `resolve_address`, and an event wait. One server presents the whole fleet.
- **SSH fallback.** `agentmux gateway ssh-command` reads
  `SSH_ORIGINAL_COMMAND`, accepts only the same operations with a strict
  argument grammar, and prints JSON. It is installed behind a forced-command
  key per host. It shares the operation code with the gRPC gateway so the two
  cannot drift, and needs a test that exercises it end to end.

### 9. Untrusted content

Transcript text can contain instructions aimed at the orchestrator. The
gateway marks every returned message as `untrusted: true` and wraps text in
a field the MCP layer renders as quoted data. It does not try to sanitize
instructions, since that is unreliable; the orchestrator's own rules decide
what to do with it.

## Proposed phases

Each phase is shippable and useful alone. Mergentic phases refer to its
`docs/roadmap.md`.

1. **CLI core and addressing.** `agentmux sessions list|status|resolve --json`,
   address parsing, fleet name-collision check. Local only. (Needed by
   mergentic phase 3 to launch or message a named agent.)
2. **Transcript reader.** Claude and opencode readers, cursor pagination,
   redaction, amp reports unsupported. Works on macOS and Linux.
3. **Safe send.** `agentmux sessions send` with refusal rules, provenance
   prefix, acknowledgement, audit log. (Mergentic phase 3 dispatch.)
4. **Gateway service.** Separate listener, tailnet bind, `whois` identity,
   allowlist, rate limit. Exposes list, read, send, status.
5. **Events.** Filtered, resumable thread-watch event stream through the
   gateway. (Mergentic phase 4 queue service.)
6. **MCP server.** `agentmux mcp` across `hosts.yaml`. (Mergentic phase 5.)
7. **SSH fallback.** Forced-command entry point sharing phase 2 to 4 code.
   (Mergentic phase 5.)

Phases 1 to 3 are usable without any network exposure, which lets mergentic's
dispatcher work against a local host first.

## Open questions

- Amp: is there an API to read and post to a thread, and does the runner accept
  messages that way? Until answered, amp is list and status only.
- Does paseo already expose read and write we should call instead of building
  our own for sessions it manages?
- `whois` requires the Tailscale local API on every host. Is bearer-token auth
  needed for hosts without it?
- Should the audit log record message text? Default no; a per-principal opt-in.
- Thread watch is not merged to `main`. This branch is based on
  `paseo-autoupdate`, which contains it, so phase 2 can reuse the collectors.
  Decide the merge order before opening a PR.
- Cursor and retention for the event stream: how much history the gateway
  keeps for a consumer that reconnects after an outage.
