# Gateway for orchestrators

Status: phases 1 to 4 are implemented; 5 to 7 are proposals.

An orchestrator agent (a person's delegate, running on one host) needs to see
and talk to agent sessions on every host. This page works out what agentmux
must provide. The orchestrator itself lives outside agentmux: agentmux
defines the contract and orchestrators integrate against it.

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
| amp | `amp threads export <id>` (JSON) or `amp threads markdown <id>` | Content is server-side; read it through the CLI with the instance's `AMP_API_KEY` (the `op run` env-file from `docs/amp-secrets.md`). `amp threads list --json` gives id, title, updated time, working tree and message count but not the runner; the export's `env.initial.runnerID` maps a thread to the instance whose `AGENTMUX_AMP_RUNNER_ID` matches. Cache that mapping per thread id, since exports of long threads are large. |

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
  tmux pane, so amp send goes through the CLI, not tmux:
  `amp threads continue <id> -ox "<message>"` posts to an existing thread and
  runs it on that thread's own executor (the runner), and
  `amp -x "<message>" --executor runner:<id> [--runner-dir <dir>]` starts a
  new thread on a runner. Both need `AMP_API_KEY` and stdin closed (with stdin
  left open the CLI waits and fails with "Timeout while reading from stdin").
  Verified on 2026-10-02 against amp 0.0.1789646488: both messages appeared in
  the thread export and in the runner's `no-tui.log`.

### 3. Status is too coarse

`Instance.Status` is running, idle, or dead. The orchestrator and the queue
service need: turn in progress, awaiting user (and what kind: question,
permission, plan), last turn error kind, usage-limit, auth failure, last
activity. Thread watch already derives these; expose them per session from its
store instead of recomputing. For amp, the thread export also carries
`meta.lastKnownAgentState.state` (for example `idle`).

### 4. No principal, allowlist, rate limit, or audit

The gateway needs, for sends:

- A caller identity from the tailnet peer: Tailscale `whois` on the
  connection's remote address, through LocalAPI in Go
  (`tailscale.com/client/local`) with `tailscale whois --json` as the
  fallback. It needs no shared secret. Every fleet host is on the tailnet, and
  `whois` works on the macOS hosts (checked on Tailscale 1.102) and on Linux.
  Identify a tagged node by node name and tags, since `whois` reports its user
  as the tagged-devices placeholder. No bearer-token path is needed; add one
  only if a host off the tailnet ever joins.
- Authorization from one Tailscale grant covering every host. It opens the
  gateway port and attaches the app capability
  `<owned-domain>/cap/agentmux-gateway`, using a domain the operator controls
  (Tailscale requires the `{domain}/{path}` form; the domain is only a
  namespace and is never contacted). The capability arrives in the `whois`
  response's `CapMap`, so the allowlist lives in the tailnet policy, not in
  per-host files. Default deny when the capability is absent.

  Each value entry pairs operations with address globs; a request is allowed
  if any entry matches. Operations are `list`, `read`, `status`, `events` and
  `send`:

  ```json
  "grants": [{
    "src": ["tag:orchestrator"],
    "dst": ["tag:agentmux-host"],
    "ip":  ["tcp:<gateway-port>"],
    "app": {
      "<owned-domain>/cap/agentmux-gateway": [
        {"ops": ["list", "read", "status", "events"], "sessions": ["*@*"]},
        {"ops": ["send"], "sessions": ["web*@*"]}
      ]
    }
  }]
  ```

  Tagging the hosts (`tag:agentmux-host`) keeps the one grant stable as hosts
  join; listing them by name also works.
- A per-principal rate limit.
- An append-only audit log (JSON lines): time, principal, target, message
  length and SHA-256, outcome. It never records message text.

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

`<host>` is the host's name in `hosts.yaml`, lower-cased. The `local` entry
(and the implicit local host when there is no `hosts.yaml`) becomes this
machine's host name up to the first dot, so `build-box.lan` is
`build-box`. Name the other `hosts.yaml` entries after their tailnet
machine names so every client produces the same addresses. Host names must be
unique once canonicalized; `agentmux` refuses a `hosts.yaml` where two
entries collide. Needed: a resolver (`agentmux resolve <address>`, and the same in
the gateway) returning host, instance, agent, workdir, and current thread; and
a check that fails loudly when two hosts claim one name. Rename already exists
(`RenameInstance`). Where the registry lives: each host is authoritative for its own instance names, and
the fleet view is `hosts.yaml` plus a fan-out query. No central store.

paseo names its own agents separately. It already provides list, read, send
and status for the agents it runs: CLI `paseo ls`, `logs`, `send`, `inspect`,
`wait`, and an MCP server with `list_agents`, `get_agent_activity`,
`send_agent_prompt` and `get_agent_status`, reachable across hosts with
`--host` (TCP, SSH, or a pairing URL). It does not drive agentmux's tmux
sessions, so the gateway cannot reuse it for them. `paseo import` resumes a
provider session as a new Paseo agent, which would put a second process on the
same session, so it is not a bridge either. The split:

- Paseo-run agents: the orchestrator uses Paseo directly. Paseo's MCP also
  exposes `respond_to_permission`, which would let the orchestrator approve
  on the person's behalf; remove it with the provider's `disabledTools` for
  the orchestrator. A Paseo daemon with password auth needs the password for
  CLI queries, resolved through `op run`.
- agentmux sessions: the gateway.

The gateway's `list_sessions` can include Paseo agents read-only later, so the
orchestrator sees one inventory; not needed for the first phases.

### 6. Events

`StreamEvents` carries instance status changes only. The queue service and
dispatcher need turn-end, awaiting-user, error and limit events. Expose thread
watch's event stream through the gateway as a filtered server stream, with a
cursor so a reconnecting consumer resumes without loss.

Thread watch already persists events as per-day JSON-lines files under
`~/.local/state/agentmux/threadwatch` and prunes days older than 14 days. The
cursor is the day file plus byte offset. A consumer can resume anywhere in
that window; a cursor older than the window gets a `gap` marker and resumes
from the oldest retained event. No separate event store.

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

Each phase is shippable and useful alone.

1. **Addressing.** Done: `internal/address` parses and prints addresses;
   `agentmux list -json` includes each session's `address`;
   `agentmux sessions resolve [-json] <address>` returns the session it names;
   `hosts.yaml` entries that collide are rejected. Richer status lands in the
   gateway phases.
2. **Transcript reader.** Done: `internal/transcript` reads Claude Code
   JSONL, opencode SQLite (through the `sqlite3` CLI) and amp threads
   (through `amp threads export`, with a thread-to-runner cache at
   `~/.cache/agentmux/amp-thread-runners.json`), redacted and paged by cursor.
   CLI: `agentmux sessions threads|read <address>`, local host only until
   phase 4. Follow-ups: keyset paging in SQL for large opencode sessions
   (a 2,500-message session takes about 5 s); amp listing is slow on a cold
   cache (tens of seconds, capped at 20 exports per call); a per-thread state
   on the `Reader` interface so status (gap 3) needn't special-case amp's
   `AmpThreadState`.
3. **Safe send.** Done, local host only until phase 4. Contract:

   ```text
   agentmux sessions send -by PRINCIPAL [-via relayed|dispatched|sent] [-from REF]
                          [-correlation ID] [-wait DUR] [-confirm DUR] [-json]
                          <instance>@<host>[#<thread>] (TEXT | -file PATH|-)
   ```

   Flags go before the address. The message is delivered as
   `[<via> by <principal>[ from <ref>]]`, a newline, then the text. Tokens
   are 1 to 64 of letters, digits and `._:@/-`; the verbs are fixed so a
   message can't claim to be an approval. Text is at most 64 KiB of UTF-8
   with no control characters other than newline and tab.

   - TUI agents (claude-code, opencode, kilo): the pane must be ready. The
     text goes in as one bracketed paste through the daemon's new `SendText`
     RPC (so newlines don't submit early), then Enter. `confirmed` is true
     when the session is seen starting a turn within `-confirm` (15 s).
   - amp: with a thread, `amp threads continue <id> --orb-execute`, after
     checking the thread is on the instance's runner and idle; without one,
     a new thread on the runner (`--executor runner:<id>`), whose id comes
     back in `thread`.
   - `-wait` polls a busy session until it finishes; without it, busy is an
     immediate refusal.
   - Every attempt after validation appends to
     `~/.local/state/agentmux/send-audit.jsonl` (0600): time, principal,
     verb, source, address, thread, correlation, byte count, SHA-256 of the
     delivered text, outcome. Never the text. Sending is refused if the log
     can't be written.

   `-json` prints the same object on success and refusal; exit 0 delivered,
   1 refused or failed, 2 usage:

   ```json
   {"ok": true, "address": "a@h", "agent": "opencode", "thread": "",
    "submitted_at": "2026-10-03T09:50:34Z", "confirmed": true,
    "bytes": 114, "sha256": "…", "correlation": "TASK-4"}
   {"ok": false, "address": "a@h", "agent": "opencode", "reason": "busy",
    "retryable": true, "detail": "a is mid-turn", "confirmed": false, …}
   ```

   Reasons, stable: `invalid`, `not_found`, `not_local`, `dead`, `busy`
   (retryable), `prompt` (needs a person), `draft` (someone has unsent text
   in the input box), `unsupported`, `failed` (retryable). Readiness comes
   from the pane: each TUI's busy footer, Claude's numbered choice menu or
   "Esc to cancel", opencode/kilo permission dialogs, and Claude's input
   line. These are heuristics tied to each TUI's current look; tests pin
   them. In phase 4 the gateway sets the principal from `whois` instead of
   trusting `-by`.
4. **Gateway service.** Done. `agentmux gateway run|install` serves list,
   status, threads, read and send as HTTP+JSON on the host's tailnet address
   (`internal/gateway`, contract in `internal/gatewayapi`). The caller is
   identified with `tailscale whois`; access comes only from the grant's app
   capability (fail closed); send takes its principal from that identity;
   sends and other ops are rate limited per principal. Clients reach it
   through a `gateway:` URL per host in `hosts.yaml`, and
   `agentmux sessions status|threads|read|send` route other hosts' addresses
   there (`internal/gatewayclient`). Shared operations live in
   `internal/ops`. Not yet exercised with a real tailnet grant, or as an
   installed service. Operator guide: [../gateway.md](../gateway.md).
5. **Events.** Filtered, resumable thread-watch event stream through the
   gateway.
6. **MCP server.** `agentmux mcp` across `hosts.yaml`.
7. **SSH fallback.** Forced-command entry point sharing phase 2 to 4 code.

Phases 1 to 3 are usable without any network exposure, which lets an
orchestrator work against a local host first.

## Resolved questions

Checked on 2026-10-02 with the CLIs on a fleet host and the vendors' docs.

- **Amp read and send.** Yes, through the CLI with an access token: export or
  markdown to read, `threads continue -ox` to send to a runner thread,
  `--executor runner:<id>` to start one. Verified live. See gaps 1 to 3.
- **Paseo reuse.** Use Paseo for agents Paseo runs; it cannot drive agentmux's
  tmux sessions. See gap 5.
- **Hosts without the Tailscale local API.** None: every fleet host is on the
  tailnet and `whois` is available through LocalAPI or the CLI. No bearer
  token. Authorization comes from a grant app capability. See gap 4.
- **Audit log message text.** Not recorded. Length and hash only.
- **Thread watch on `main`.** It is merged; this branch is based on
  `origin/main`.
- **Event retention.** Reuse thread watch's 14-day day files and a file and
  offset cursor. See gap 6.

- **Capability name and grant.** A domain the operator owns; one grant
  covers all hosts. Value format in gap 4.
- **Listing amp threads per runner.** The CLI has no runner filter, so scan
  `amp threads list --json` and map each thread to a runner through a cached
  export (`env.initial.runnerID`). Only threads updated since the last scan
  need a new export.
