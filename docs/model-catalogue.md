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
        {"host": "local", "available": true, "checked_at": "2026-01-01T00:00:00Z"},
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
