package probe

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/m-rk/agentmux/daemon/internal/catalogue"
	"github.com/m-rk/agentmux/daemon/internal/runas"
)

// The amp check runs `amp usage`: it needs no inference and makes no
// thread, so it pushes nothing to the Amp phone app. Its output carries
// the account's email on the "Signed in as" line; only the fact that the
// line is there is read, and none of the output is kept.

var ampCredit = regexp.MustCompile(`(-)?\$\s*(-?[0-9][0-9,]*(?:\.[0-9]+)?)\s+remaining`)

// parseAmpUsage reads `amp usage`: nil when signed in with credit left on
// any balance, else a Failure whose words are fixed.
func parseAmpUsage(out []byte) *Failure {
	signedIn, credit := false, false
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), maxOutput)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "Signed in as ") {
			signedIn = true
			continue
		}
		m := ampCredit.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		v, err := strconv.ParseFloat(strings.ReplaceAll(m[2], ",", ""), 64)
		if err == nil && m[1] == "" && v > 0 {
			credit = true
		}
	}
	switch {
	case !signedIn:
		return &Failure{Kind: catalogue.ErrKindOther, Words: "not signed in"}
	case !credit:
		return &Failure{Kind: catalogue.ErrKindOther, Words: "no credit left"}
	}
	return nil
}

// AmpThreadPath is where the AMUX-141 standing probe thread was kept.
// The probe no longer makes threads; RetireAmpThread archives that one.
func AmpThreadPath(home string) string {
	return filepath.Join(home, ".local", "state", "agentmux", "amp-probe-thread.json")
}

// RetireAmpThread archives the old standing probe thread, if a state
// file names one, and removes the file. It runs once: on failure the
// file stays and the next probe tries again. log gets one line.
func RetireAmpThread(ctx context.Context, path string, archive func(ctx context.Context, id string) error, log func(string)) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var st struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(b, &st) == nil && st.ID != "" {
		if err := archive(ctx, st.ID); err != nil {
			log("could not archive the old amp probe thread " + st.ID + "; will try again next probe")
			return
		}
		log("archived the old amp probe thread " + st.ID)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log("could not remove " + path)
	}
}

// ArchiveAmpThread runs `amp threads archive id` as the current user. A
// thread amp no longer has counts as archived.
func ArchiveAmpThread(ctx context.Context, id string) error {
	cctx, cancel := context.WithTimeout(ctx, TryTimeout)
	defer cancel()
	cmd := runas.CurrentUserCommandContext(cctx, "amp", "threads", "archive", id)
	cmd.Env = probeEnv(cmd.Env)
	out, err := cmd.CombinedOutput()
	if err != nil && strings.Contains(strings.ToLower(string(out)), "does not exist") {
		return nil
	}
	return err
}
