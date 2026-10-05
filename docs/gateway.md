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
| `ops`      | any of `list`, `status`, `threads`, `read`, `send`, `create`, `run`, `events` |
| `sessions` | `path.Match` patterns over `<instance>@<host>`; `*` does not match `/` |

Entries add up: a call is allowed if any one entry allows it. There is no
deny. `create` starts an agent and a Git worktree on the host, so it is never
implied by another op: grant it explicitly, with a session pattern that limits
the names it may create (`task-*@build-box` above). `ip: tcp:4288` is the network path; the `app` capability is the
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

## Running an amp thread

`run` starts an amp thread on an amp instance (or continues one with a
thread id), for prompts that should become their own thread rather than a
paste into the runner's TUI. It shares `send`'s rate bucket, and refuses
with a non-2xx status like `create`. See [Starting amp
threads](amp-run.md).

```json
POST /v1/run
{"address": "site-amp@build-box", "text": "do the thing", "title": "AMUX-17 do the thing"}
```

| field     | meaning |
|-----------|---------|
| `address` | `<instance>@<host>` to start a thread, or with `#<thread>` to continue one; the grant is checked against the session without the thread suffix |
| `text`    | the prompt |
| `title`   | names a new thread (`<task id> <task name>`); ignored when continuing |

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
