# fake amp

Test double for the `amp` CLI (AMUX-49). Records argv (`FAKE_AMP_ARGV_LOG`)
and prints canned text; it never touches a network, a thread or a model.
Put this directory on `PATH` in tests. Task instances' amp wrapper refuses a
real `-x` unless `AGENTMUX_ALLOW_LIVE_AMP=1`, so a test that needs to run
`-x` points at this fake instead.

See [docs/amp-run.md](../../../docs/amp-run.md).
