# fake codex

Test double for the `codex` CLI (AMUX-55). Prints canned, synthetic `--json`
JSONL from `fixtures/` and never touches a network or a model. Put this
directory on `PATH` in tests; see the header of `codex` for the scenario
and override env vars (`FAKE_CODEX_SCENARIO=success|turn_failed|error_item|
rate_limit|hang`).
