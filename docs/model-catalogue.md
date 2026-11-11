# Model catalogue

agentmux keeps one catalogue of what each runner can be launched as. Each
entry (an *offering*) has a runner, a model id and version, a short label, a
family, the effort levels and speeds it accepts, the fuel tank it draws from,
a difficulty tier, and the hosts it is on. `agentmux models list -json`
returns the catalogue with a live availability check per host.

The repo holds the format, the checks and a redacted example
([`examples/catalogue.example.yaml`](examples/catalogue.example.yaml)). The real
entries live in `~/.config/agentmux/catalogue.yaml` on the host that runs the
command.

## JSON shape

```json
{
  "version": 1,
  "offerings": [
    {
      "id": "claude-large",
      "runner": "claude-code",
      "icon": "claude",
      "model": "example-large-1",
      "version": "1",
      "label": "Large",
      "family": "example-large",
      "efforts": ["low", "medium", "high"],
      "speeds": ["standard", "fast"],
      "fuel": "claude",
      "tier": "hardest",
      "hosts": [
        {"host": "local", "label": "box-1", "available": true, "checked_at": "2026-01-01T00:00:00Z"},
        {"host": "host-a", "available": false, "reason": "host unreachable", "checked_at": "2026-01-01T00:00:00Z"}
      ]
    }
  ],
  "fuel": [{"name": "chatgpt", "cap_percent": 50, "cap_source": "manual"}]
}
```

- `runner`: `claude-code`, `codex`, `amp`, `opencode` or `kilo`.
- `icon`: a stable key per runner (`claude`, `codex`, `amp`, `opencode`,
  `kilo`) for UIs to map to an icon.
- `fuel`: `claude`, `chatgpt`, `amp` or `provider`. `tier`: `easy`, `hard` or
  `hardest`.
- `mode`: only on `amp` offerings. Each Amp mode is its own offering, and a
  launch passes it instead of the host-wide mode in `amp.yaml`.
- `instances`: existing instances that carry the offering (opencode and kilo fix
  provider and model when an instance is created).
- `hosts[]`: one live result per configured host. A check that failed is
  listed with `available: false` and a `reason`; an entry is never dropped.
- Fields are only ever added. Readers must ignore fields they don't know.

### Launching a speed

An offering's `speeds` are what a launch may ask for. For a Claude offering,
`fast` is passed through `sessions create -speed` / `sessions run -speed`,
which sets the session's `fastMode` setting at start; see
[Fast mode](gateway.md#fast-mode). Whether it took is checked from the
session's own replies, not assumed. Codex and Amp instances refuse `-speed`.

## Live checks

`agentmux models list -json` returns `{"sources": [...], "catalogue": {...}}`.
For each host named in `hosts`, it asks:

- this machine (`local`, or its own hostname): which runner executables are on `PATH`;
- any other host: the host's gateway `runners` op, from `hosts.yaml`. The gateway
  grant needs `runners` in its ops list. A host without a gateway, a gateway
  that predates the op, a missing grant or an unreachable host is reported as
  unavailable with the reason; the entry stays.

`host` is the name as written in config (a `hosts.yaml` name, or `local`);
`label` is a display name, which for `local` is the short hostname. Amp modes
are not verified: an Amp offering is available when `amp` is installed.

## Amp modes per launch

Each Amp mode is its own offering with a `mode`. A launch passes it instead of
the host-wide `mode:` in `amp.yaml`: `agentmux sessions run -mode MODE`, or
`-amp-mode MODE` when creating an instance.

## Fuel

`fuel[]` records caps that can't be read from a provider as `cap_source:
manual`. When Amp is installed, the `chatgpt` tank also gets `subscription`:
`present`, `absent` or `unreadable: <why>`, from `amp config model-providers list`.
That output is read only after a check that no key appears in the clear
(an unmasked-looking word makes the read fail and the output is dropped); only
the yes/no is kept, and no key is ever stored or printed.
