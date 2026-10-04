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
   sessions those grants' patterns match.
5. Rate limits apply per principal: sends 30 a minute (burst 10), everything
   else 300 a minute (burst 60).

It fails closed. If whois fails, the node has no name, the capability is
missing, or its value is not an array, every request gets `403 forbidden`.
A grant with an unknown op name, no ops, no sessions, or a bad pattern is
ignored whole.

Replies: a problem with the request as a whole (unknown caller, bad route,
wrong method, bad or oversized body) is a non-2xx `{"error": {"reason", "detail"}}`.
For `send`, a request that parsed and was then refused (`forbidden`,
`rate_limited`, or any reason `ops.Send` gives, such as `busy`) is HTTP 200
with the same result object a delivered send has, `ok: false` and a `reason`.
Every other operation refuses with a non-2xx status from
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
        {"ops": ["send"], "sessions": ["web*@*"]}
      ]
    }
  }
]
```

Each entry of the array is a grant object:

| field      | meaning |
|------------|---------|
| `ops`      | any of `list`, `status`, `threads`, `read`, `send`, `events` |
| `sessions` | `path.Match` patterns over `<instance>@<host>`; `*` does not match `/` |

Entries add up: a call is allowed if any one entry allows it. There is no
deny. `ip: tcp:4288` is the network path; the `app` capability is the
authorization.

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

`agentmux sessions status|threads|read|send` and `agentmux list` then reach
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
