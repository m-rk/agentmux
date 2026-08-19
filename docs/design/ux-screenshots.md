# Deterministic UX screenshots

Generate every instance-wizard screenshot from the repository root:

```sh
./scripts/generate-ux-screenshots.sh
```

Verify that the checked-in screenshots are current without rewriting them:

```sh
./scripts/generate-ux-screenshots.sh -check
```

The generator imports the same `internal/wizardui` form builders used by the
live wizard, fills them with fixed synthetic values, and renders their styled
static views directly to SVG. It does not start a daemon, open a terminal,
inspect the local user or hostname, read session state, or need a browser and
therefore cannot carry machine-derived details into a public artifact.

Use `-list` to see the available scenarios, or regenerate just one:

```sh
./scripts/generate-ux-screenshots.sh -list
./scripts/generate-ux-screenshots.sh -scenario kilo-custom
```

Add new scenarios in `daemon/cmd/uxshot/main.go`. Reuse the production form
groups from `internal/wizardui`; do not recreate field labels or layout in the
renderer.
