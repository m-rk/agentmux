# Gateway

`agentmux gateway` is a small HTTP service on a host's tailnet address. An
orchestrator on another tailnet host can list that host's agent sessions,
check whether one can take a message, read its transcript, and send it a
message. Who may do what is decided by the tailnet policy, not by anything
stored on the host. Design: [design/gateway.md](design/gateway.md) (phase 4).

The wire format is in `daemon/internal/gatewayapi`. The server is
`daemon/internal/gateway`.

## How a request is handled

1. The gateway only listens on a tailnet address (`100.64.0.0/10` or
   `fd7a:115c:a1e0::/48`). It refuses to bind anything else.
2. For each request it runs `tailscale whois --json <remote ip:port>` (results
   are cached per IP for 30 seconds, so a policy change takes up to that long
   to apply). That gives the calling node's name and the app capability the
   policy grants it.
3. The node name, lower-cased, is the **principal**. It is what shows in the
   provenance prefix of a sent message and in the host's send audit log. A
   request cannot set it.
4. An operation on `name@host` is allowed when some grant lists the operation
   and has a session pattern matching `name@host` (a `#thread` suffix is
   ignored). `list` is allowed when any grant lists it, and returns only the
   sessions those grants' patterns match. `create` is checked against the
   address of the session it would create, `<new instance>@<this host>`.
5. Rate limits apply per principal: sends and creates share one bucket, 30 a
   minute (burst 10); everything else 300 a minute (burst 60).

It fails closed. If whois fails, the node has no name, the capability is
missing, or its value is not an array, every request gets `403 forbidden`.
A grant with an unknown op name, no ops, no sessions, or a bad pattern is
ignored whole.

Replies: a problem with the request as a whole (unknown caller, bad route,
wrong method, bad or oversized body) is a non-2xx `{"error": {"reason", "detail"}}`.
For `send`, a request that parsed and was then refused (`forbidden`,
`rate_limited`, or any reason `ops.Send` gives, such as `busy`) is HTTP 200
with the same result object a delivered send has, `ok: false` and a `reason`.
Every other operation, `create` included, refuses with a non-2xx status from
`gatewayapi.HTTPStatus` (400, 403, 404, 421, 429, 501, 500). `events` is
reserved and answers 501.

## Tailnet policy

One grant covers every host. Tag each host that runs the gateway
`tag:agentmux-host`, and tag or name the orchestrators however you like. The
capability name is a domain you own plus a path; `example.com` below is a
placeholder.

```json
"grants": [
  {
    "src": ["tag:agentmux-orchestrator"],
    "dst": ["tag:agentmux-host"],
    "ip":  ["tcp:4288"],
    "app": {
      "example.com/cap/agentmux-gateway": [
        {"ops": ["list", "read", "status", "threads"], "sessions": ["*@*"]},
        {"ops": ["send"], "sessions": ["web*@*"]},
        {"ops": ["create"], "sessions": ["task-*@build-box"]}
      ]
    }
  }
]
```

Each entry of the array is a grant object:

| field      | meaning |
|------------|---------|
| `ops`      | any of `list`, `status`, `threads`, `read`, `send`, `create`, `run`, `retire`, `gc`, `events` |
| `sessions` | `path.Match` patterns over `<instance>@<host>`; `*` does not match `/` |

Entries add up: a call is allowed if any one entry allows it. There is no
deny. `create` starts an agent and a Git worktree on the host, so it is never
implied by another op: grant it explicitly, with a session pattern that limits
the names it may create (`task-*@build-box` above). The same holds for
`retire` (ends a session and deletes its branch) and `gc` (deletes retired
leftovers host-wide). `ip: tcp:4288` is the network path; the `app` capability is the
authorization.

## Starting a task session

`create` makes a Git worktree and a new instance on the gateway's host, so an
orchestrator can start a task there.

```json
POST /v1/create
{"template": "web@build-box", "instance": "task-42", "branch": "feature/task-42",
 "base": "main", "worktree": "task-42", "allow_files": ["/home/me/notes/task-42.md"]}
```

| field         | meaning |
|---------------|---------|
| `template`    | an existing instance on this host; its agent, provider, model, provider base URL, API key env var name and run user are copied |
| `instance`    | name of the new instance; the grant is checked against `<instance>@<this host>` |
| `branch`      | branch the worktree is on |
| `base`        | optional branch on `origin` a new branch starts from (`main`; a leading `origin/` is accepted). It is fetched first, and the call is refused (`failed`) if the fetch fails or `origin/<base>` is missing; a local ref is never used instead. Default (no `base`): the `origin/HEAD` target, else `HEAD`, after a best-effort `git fetch origin` |
| `worktree`    | optional directory name, default the instance name |
| `allow_files` | optional absolute paths on this host the agent may read and edit, as for `agentmux new -allow-file` |

The worktree goes in `<parent of the template's repo>/<repo>-worktrees/<worktree>`,
made from the template's workdir with `git worktree add -b <branch> <path>
<start>`. With `base`, that is `git fetch origin <base>` followed by
`origin/<base>`. If the branch already exists
and is not checked out, the worktree uses it. A worktree already at that path
on that branch is reused; any other existing path is refused (`invalid`). An
instance with that name and workdir is reused; with another workdir it is
refused. The call returns when the instance is created, not when it is ready;
poll `status`.

```json
{"address": "task-42@build-box", "name": "task-42", "agent": "claude-code",
 "status": "running", "workdir": "/home/me/src/app-worktrees/task-42",
 "project": "owner/app", "branch": "feature/task-42", "created": true,
 "base": "main", "base_commit": "9fceb02d0ae598e95dc970b74767f19372d61af8"}
```

`base` and `base_commit` (the commit the worktree started from) are present
only when the request had a `base` and this call made the branch from it; a
reused instance, worktree or existing branch reports neither.

`created` is false when an existing instance was reused. A refusal is a non-2xx
`{"error": {"reason", "detail"}}`: `invalid` (names, branch, base name, worktree
path, allow-file, or an instance name clash), `not_found` (template),
`forbidden`, `rate_limited`, `unsupported` (the template's workdir is not in a
Git checkout, or its run user is not the gateway's user), `failed` (including a `base` that could not be fetched or is missing on origin).

From a shell, routed by the template's host:

```sh
agentmux sessions create -template web@build-box -instance task-42 -branch feature/task-42
agentmux sessions create -template web@build-box -instance task-42 -branch feature/task-42 -base main
```

`-base` is listed in `agentmux sessions create -h`; a caller can detect support
from that line. `-json` adds `base` and `base_commit` to the result.
`-dry-run` checks everything a real create would — including fetching
`origin/<base>` — but creates nothing; see [Deploy](deploy.md).

## Running an amp thread

`sessions send` to an amp instance never pastes into the runner's
terminal — that starts a new thread on amp's default model instead of
the host-configured mode, whose output nobody reads (AMUX-37). It
resumes the instance's thread through the `run` path instead: the
address's thread suffix, else the instance's current thread (newest
`sessions run` log, else the newest thread listed on its runner), with
the host/instance mode. With no thread to resume the send is refused as
`not_found` with the `sessions run` to use instead. The result names
the resumed thread and `confirmed` means its run reports running — poll
`sessions status` for `<instance>#<thread>` for what happens next, and
the audit log records the outcome as `resumed`, never `delivered`.
`-doorbell` is refused for amp instances.

`run` starts an amp thread on an amp instance (or continues one with a
thread id), for prompts that should become their own thread rather than a
paste into the runner's TUI. It shares `send`'s rate bucket, and refuses
with a non-2xx status like `create`. See [Starting amp
threads](amp-run.md).

```json
POST /v1/run
{"address": "site-amp@build-box", "text": "do the thing", "title": "AMUX-17 do the thing", "labels": ["agentmux-task"]}
```

| field     | meaning |
|-----------|---------|
| `address` | `<instance>@<host>` to start a thread, or with `#<thread>` to continue one; the grant is checked against the session without the thread suffix |
| `text`    | the prompt |
| `title`   | names the thread (`<task id> <task name>`); re-applied after every run, since amp's auto-title overwrites it |
| `labels`  | thread labels; every entry rides `amp -l` on the run so `amp threads list --label X` finds it |

The reply is the thread plus its state (`running` when just launched):

```json
{"ok": true, "address": "site-amp@build-box#T-11111111-1111-4111-8111-111111111111",
 "agent": "amp", "thread": "T-11111111-1111-4111-8111-111111111111",
 "thread_id": "T-11111111-1111-4111-8111-111111111111",
 "thread_url": "https://ampcode.com/threads/T-11111111-1111-4111-8111-111111111111",
 "state": "running"}
```

From a shell, routed by the instance's host:

```sh
agentmux sessions run -file prompt.md site-amp@build-box
agentmux sessions run -file followup.md -thread T-11111111-1111-4111-8111-111111111111 site-amp@build-box
```

## Retiring a task session

Once a task is done and its work is on `main`, its runner session has
served its purpose. `retire` ends it: for an amp instance it re-applies
the task title (`amp threads rename`, before the archive — amp refuses
to rename an archived thread) and archives the thread with
`amp threads archive` (the thread stays readable on
ampcode.com) and kills any in-flight `amp -x` / `threads continue` runs
for that instance, so a detached run can't keep working in the worktree
being removed; for every agent it stops the session, removes the
instance's units and registry entry, and removes the worktree. The
worktree's branch — plus the recorded and sibling `task/<ID>-*` branches
— is deleted only when origin's default branch provably contains every
commit (directly, or as a squash/cherry-pick equivalent); otherwise the
branch is kept and reported (`keep branch …: <reason>`) while the retire
still goes through — the dry run and the real retire agree. Pass
`require_merged: true` (CLI `-require-merged`) for the old stricter mode,
which refuses the whole retire (`invalid`) when any branch is unmerged.
Claude Code
transcripts are kept; an opencode session's stored rows are recorded so
`gc` can delete them later. Only `task-*` instances are ever touched —
anything else is refused (`forbidden`) — and a dirty worktree or a
detached HEAD holding commits no branch points at is refused (`invalid`)
so the caller
can raise an ask instead. A dry run reports the branch check
truthfully: it names "keep branch …" with the reason rather than
claiming containment it never verified. On Linux the privileged half
(stop, units, registry) runs through the daemon, so `sessions retire`
works unprivileged; git and worktree operations always run as the
instance's run user, never as root.

```json
POST /v1/retire
{"address": "task-42@build-box"}
```

| field     | meaning |
|-----------|---------|
| `address` | `<instance>@<host>` (no `#thread`); the grant is checked against it |
| `dry_run` | optional; list what would go without changing anything |
| `require_merged` | optional; refuse the whole retire when any branch is unmerged instead of keeping it |

From a shell, routed by the instance's host:

```sh
agentmux sessions retire task-42@build-box
agentmux sessions retire -dry-run task-42@build-box
```

## Garbage-collecting retired sessions

`gc` deletes the leftovers of retired sessions older than the host
retention (`~/.config/agentmux/retention.yaml`, default 14 days):
archived amp threads (`amp threads delete`) and stored opencode
sessions. Claude Code transcripts are never deleted. Unlike every other
op, `gc` names no session, so the grant only needs the op itself:

```json
"example.com/cap/agentmux-gateway": [
  {"ops": ["retire"], "sessions": ["task-*@build-box"]},
  {"ops": ["gc"], "sessions": ["*@*"]}
]
```

```json
POST /v1/gc
{"dry_run": true}
```

`dry_run` lists what would go. Show the operator a first `gc -dry-run`
to approve before the first real collection. From a shell (every known
host, or one with `-host`):

```sh
agentmux gc -dry-run
agentmux gc -dry-run -host build-box
```

### Sweeping junk amp threads

Retire and gc only reach threads tied to a task instance through a
retired record — but relay strays (`[relayed by ...]` sends that never
reached a live worker) and probe threads (`"ok"`, `"reply with
exactly ..."`) belong to no instance at all, so nothing could ever
archive them. `agentmux amp sweep` covers that gap: it lists this
host's amp account threads and archives the clearly-junk ones, never
deleting anything itself:

- a first message starting `[relayed by` or `[sent by`, older than an
  hour (a stray owned by no instance);
- 6 messages or fewer, older than 6 hours, and neither a dispatched
  worker thread nor a nightly review (an abandoned probe);
- an untitled thread in error state older than an hour;
- a nightly review thread older than 3 days.

It never touches a thread with more than 6 messages, a dispatched
worker of a live task (`[dispatched by ...]` prefix, or recorded for a
live `task-*` instance), or anything younger than the limits above.
Archiving is reversible (`amp threads archive --unarchive`), and every
archived thread is printed with its reason — the same output with
`-dry-run` lists without archiving. The daily gc timer runs the sweep
first (remote hosts sweep through their own timer), then this same gc
deletes the swept threads after the same retention, so archived junk
of either kind ages out together:

```sh
agentmux amp sweep -dry-run
agentmux amp sweep
```

The local gc pass runs the sweep first (remote hosts sweep through
their own daily gc timer): junk threads belong to no task instance,
so no retired record could ever reach them — the sweep archives them,
records each under `~/.local/state/agentmux/swept/`, and this same gc
deletes them after the same retention.

## Running it

```sh
agentmux gateway run -capability example.com/cap/agentmux-gateway
```

Flags:

| flag | default | |
|------|---------|--|
| `-capability NAME` | required, except with `-insecure-test-grants` | the app capability name from the policy |
| `-listen ADDR` | `tailscale ip -4` on port 4288 | must be a tailnet address |
| `-socket PATH` | the platform's `agentmuxd` socket | |
| `-tailscale PATH` | `tailscale` | CLI used for whois and `ip -4` |
| `-send-rate N/min`, `-send-burst N` | 30/min, 10 | per principal; `0` turns the limit off |
| `-rate N/min`, `-burst N` | 300/min, 60 | per principal, all other ops |
| `-insecure-test-grants FILE` | off | see below |

The process needs the `agentmuxd` socket and the ability to run
`tailscale whois`. On Linux, as a non-root user, run
`sudo tailscale set --operator=USER` once.

## Installing as a service

```sh
agentmux gateway install -capability example.com/cap/agentmux-gateway [-listen ADDR] [-bin PATH] [-print]
```

- macOS: a per-user LaunchAgent, `com.m-rk.agentmux.gateway`, with
  `RunAtLoad` and `KeepAlive`. No sudo. The binary defaults to
  `~/.agentmux/bin/agentmux`. Logs: `~/.agentmux/log/gateway.log` and
  `gateway.err.log`.
- Linux: `agentmux-gateway.service`, run as `-run-user` (default: the user
  that owns the sessions, as for `doctor`), `Restart=always`. Run as root.
  Logs: `journalctl -u agentmux-gateway`.

`-print` shows the unit without installing it. The installer records the
absolute path of the `tailscale` CLI it finds, since a service starts with a
minimal `PATH`. Without `-listen` the service looks up the tailnet address on
each start, so it waits out tailscale coming up late (it exits and is
restarted until `tailscale ip -4` answers).

## Trying it without a tailnet policy

```sh
cat > /tmp/test-grants.json <<'JSON'
[{"ops": ["list", "status", "read", "threads"], "sessions": ["*@*"]},
 {"ops": ["send"], "sessions": ["my-test-instance@*"]}]
JSON
agentmux gateway run -listen 127.0.0.1:4288 -insecure-test-grants /tmp/test-grants.json
curl -s -X POST 127.0.0.1:4288/v1/list -d '{}'
```

In this mode the gateway binds loopback only (any other address is refused),
does no whois, treats every request as principal `loopback-test` with the
grants in the file, and logs a warning at start. Anyone who can reach the
loopback port as any local user gets those grants. Do not use it in a service.

## Reaching another host

On the calling host, give each other host a `gateway:` URL in
`~/.config/agentmux/hosts.yaml`. Name the entry after that host's own
agentmux host name (its short hostname), so addresses match:

```yaml
hosts:
  - name: local
    address: unix:///run/agentmux/agentmuxd.sock
  - name: build-box
    gateway: http://100.64.0.2:4288
```

`agentmux sessions status|threads|read|send|create|run` and `agentmux list` then reach
`build-box` through its gateway. An `address:` (the daemon's own port) is
optional; without one, the TUI and the commands that manage instances skip
that host.

## Logs

One line per request on stderr:

```text
gateway: principal=orch-box op=send target="web-opencode@hostA" status=200 result=ok remote=100.64.0.9:51234 took=1.5s
```

`result` is `ok` or the refusal reason. Message text and transcript content
are never logged; sends are audited by length and hash in the host's send
audit log, with the principal.

## Troubleshooting

- `403 forbidden: caller is not a known tailnet node`: `tailscale whois`
  failed. Run `tailscale whois <caller ip>` as the service user. Check the
  CLI is reachable (`-tailscale`) and, on Linux, that the user is the
  tailscale operator.
- `403 forbidden: caller has no agentmux gateway capability`: the policy has
  no grant for this source and destination, the capability name differs from
  `-capability` (compare with the `CapMap` of
  `tailscale whois --json <caller ip>` on the host), or every entry is
  invalid. Policy changes can take 30 seconds to be seen.
- `forbidden` on one operation: no entry lists that op with a pattern
  matching the session. The address is `<instance>@<host>` with the host
  lower-cased.
- `refusing to bind` / `not a tailnet address`: pass a `-listen` inside the
  tailnet ranges.
- `421 not_local`: the address names another host than this one.
- `429` / `rate_limited`: the principal is over its limit; `Retry-After`
  says when to retry. Raise `-rate` / `-send-rate` if needed.
- Connection refused or timeout from the orchestrator: the grant's `ip`
  rule must allow `tcp:4288` and the service must be running.
