# Per-file access: `agentmux new -allow-file`

Lets an agent read and edit a few individual files outside its workdir, such as a
task note in a notes vault, without opening the directories they live in.

```sh
agentmux new -y -instance task-4 -agent claude-code \
  -workdir /path/to/worktree \
  -allow-file "/path/to/vault/tasks/TASK-4 Some title.md"
```

`-allow-file` is repeatable (at most 20). Tools can detect support by looking for
`-allow-file` in `agentmux new -h`.

## What it grants

Read and Edit on exactly that file. Not its directory, and not the workdir's
tracked files: the grant lives in a settings file outside the workdir, so there
is nothing for the agent to commit.

Paths are checked when `new` runs:

- absolute and already clean (no `..`, `.`, `//`, trailing `/`)
- an existing regular file, not a directory
- not inside the instance workdir (already accessible there)
- symlinks are resolved and the real target is what gets granted
- no `?`, `{`, `}`, backslash or control characters in the path (these are
  pattern syntax in agent permission rules and can't be escaped reliably)

The list is stored in the instance registry as `AGENTMUX_ALLOW_FILES` (a one-line
JSON array), so restarts keep it. Re-running `new -y` without the flag clears it,
like every other field. The change applies when the session next starts; a
running session isn't restarted.

At session start, a file that has since been renamed or deleted is skipped with a
warning in the instance log instead of failing the launch.

## Per agent

### claude-code

At each session start agentmux writes `~/.agentmux/.settings/<instance>.claude.json`
(directory 0700, file 0600, owned by the instance's user) and launches
`claude ... --settings <that file>`. Instances without allow-files launch with the
same arguments as before, and any stale settings file is removed.

The file contains `Read(//abs/path)` and `Edit(//abs/path)` allow rules (`//` is an
absolute filesystem path in Claude Code rules; a single `/` is relative to the
settings file). Spaces need no escaping; `[ ] ( ) *` are backslash-escaped.

Behavior checked against Claude Code 2.1.288 with `claude -p`:

- Allow rules alone grant exactly the file in default mode: sibling files are
  denied. In auto mode the classifier decides for anything not covered by a rule,
  and in a scratch directory it approved siblings too, so auto mode is not a
  boundary on its own; the allow rule's job there is to make the exact file
  unconditional.
- `permissions.blockReadsOutsideWorkingDirectories` (what "No, and block reads
  outside the working directories from now on" in the read prompt writes to user
  settings) is **not** overridden by allow rules, by a PreToolUse hook that
  returns allow, or by pointing `additionalDirectories` at the file. Only a
  working or additional directory re-opens reads.
- `?` can't be escaped (`\?` and `[?]` don't match the literal), `\*` does.

So when agentmux sees the block set in the instance user's
`~/.claude/settings.json` or in the workdir's `.claude/settings.json` /
`.claude/settings.local.json` at session start, it additionally adds the file's
parent directory as an additional directory and writes Read and Edit deny rules
for every other entry in that directory (subdirectories get a `/**` rule too).
Verified in auto mode: the note is readable and editable, siblings and files in
sibling subdirectories are denied.

Limits of that fallback:

- Entries created in the directory after the session started are not fenced
  until the next session start.
- Deny rules cover Claude's file tools; a shell command run by the agent
  (`cat`) is governed by the Bash permission rules and sandbox, not these.
- Managed (admin) settings aren't inspected for the block.
- A directory with more than 5000 entries can't be fenced, and its grant is
  dropped with a warning.

### opencode and kilo

Verified with opencode 1.18 and kilo 7.4 (`run`). Both check reads outside the
workdir with an `external_directory` permission, which only ever matches a
directory (`<dir>/*`), so a single file can't be allowed there. The generated
config therefore:

- allows `external_directory` for the note's directory;
- sets the `read` and `edit` rules (matched against the path relative to the
  workdir) to deny that directory and then allow the note itself; the last
  matching rule wins, so the order matters.

The note is readable and editable; siblings are denied to the file tools. It
is passed as `OPENCODE_CONFIG_CONTENT` / `KILO_CONFIG_CONTENT`, which merges
over the project config agentmux writes, so nothing lands in the worktree.
It reaches the agent through the instance's tmux server environment, so a
changed grant applies after the tmux server restarts.

Limits:

- Once the directory passes `external_directory`, the agent's shell commands
  (`cat`, `ls`) can reach siblings. Nothing in their config prevents that.
- `*` and `?` can't be escaped in their rules, so paths containing them (or
  `{env:`/`{file:` substitutions) are refused rather than granted too widely.

### amp

amp doesn't restrict file access outside its working directory at all
(verified with `amp -x`: a sibling outside the workdir was read and edited
with no config), so it needs no grant and `-allow-file` cannot confine it.
`agentmux new -y` says so in its result message.

### zero

Stored in the registry, not applied. `agentmux new -y` prints a warning.

## Code

- `daemon/internal/allowfiles`: validation, registry encoding, and the
  per-agent renderers. `Supported(agent)` is the list of agents that apply the
  grant; new agents add a case there plus a renderer.
- `daemon/internal/session/claudecode.go`: `prepareClaudeAllowSettings` and
  `claudeLaunchArgs`.
- `daemon/internal/session/agentmux.go`: `allowFilesEnv` for opencode and kilo.
