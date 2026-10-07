package session

import (
	"os"
	"path/filepath"
	"strings"
)

// AllowLiveCodexEnv is the opt-in that lets a task instance run a real
// `codex exec` (AMUX-55, like AMUX-49 for live Discord calls). Unset, the
// task wrapper refuses.
const AllowLiveCodexEnv = "AGENTMUX_ALLOW_LIVE_CODEX"

// taskCodexStubRefusal is the one line the wrapper prints and logs when it
// refuses; the wrapper script and the tests quote the same text.
const taskCodexStubRefusal = "refusing live codex exec in a task instance (" + AllowLiveCodexEnv + "=1 to allow); tests use daemon/testdata/fakecodex"

// taskCodexWrapper sits first on PATH in task-* panes next to the amp
// wrapper. `codex exec` and `codex exec resume` spend real quota, so they
// are refused unless AGENTMUX_ALLOW_LIVE_CODEX=1; every other call (login
// status, --version, ...) passes through untouched. The refusal goes to
// stderr and, as one line, to $AGENTMUX_TASK_LOG (default
// codex-refused.log beside the wrapper). The body has no % directives.
const taskCodexWrapper = `#!/bin/sh
# agentmux task wrapper (AMUX-55): refuse live codex exec. Generated - do
# not hand-edit; the runner rewrites it when its content drifts.
SELF_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
if [ "${@@ALLOW_ENV@@:-}" != 1 ]; then
	for a in "$@"; do
		case "$a" in
		--) break ;;
		exec)
			MSG="codex: @@REFUSAL@@"
			echo "$MSG" >&2
			echo "$MSG" >> "${AGENTMUX_TASK_LOG:-$SELF_DIR/codex-refused.log}" 2>/dev/null
			exit 1 ;;
		esac
	done
fi
REAL=""
REST="$PATH:"
while [ -n "$REST" ]; do
	d=${REST%%:*}
	REST=${REST#*:}
	[ -z "$d" ] && d="."
	if [ "$d" != "$SELF_DIR" ]; then
		if [ -x "$d/codex" ] && [ ! -d "$d/codex" ]; then
			REAL="$d/codex"
			break
		fi
	fi
done
if [ -z "$REAL" ]; then
	echo "codex: no real codex behind the wrapper on PATH" >&2
	exit 1
fi
exec "$REAL" "$@"
`

func taskCodexWrapperFor() string {
	s := strings.ReplaceAll(taskCodexWrapper, "@@ALLOW_ENV@@", AllowLiveCodexEnv)
	return strings.ReplaceAll(s, "@@REFUSAL@@", taskCodexStubRefusal)
}

// ensureTaskCodexStub writes the codex wrapper into dir, rewriting it when
// its content drifts.
func ensureTaskCodexStub(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	script := taskCodexWrapperFor()
	path := filepath.Join(dir, "codex")
	if data, err := os.ReadFile(path); err == nil && string(data) == script {
		return nil
	}
	return os.WriteFile(path, []byte(script), 0o755)
}
