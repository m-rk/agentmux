// The amp-backed doctor escalation: ClaudeAnalyzer's twin (claude.go),
// driven through a single `amp -x` call instead of `claude -p
// --json-schema`. Same claudeSystemPrompt, same Snapshot payload, same
// Plan parsing (decodePlan) — only the transport and the JSON-only
// instruction differ, because amp has no --json-schema flag of its own.
// See docs/doctor.md's "-checker amp" for the operator-facing picture and
// daemon/internal/ampexec for the shared mechanics/safety guarantees.
package dailycheck

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/m-rk/agentmux/daemon/internal/ampexec"
)

// ampJSONInstructions spells out planSchema() (claude.go) for amp, in
// prose, since amp has no structured-output flag to enforce it. Keep this
// in lockstep with planSchema/decodePlan.
const ampJSONInstructions = "Reply with ONLY a single JSON object matching this exact shape (a ```json code fence around it is fine; nothing else is):\n\n" +
	`{"summary": "string", "findings": [{"instance": "string", "notable": true, "finding": "string", "evidence": "string", "action": "none|start|restart|send_escape", "reason": "string"}]}` +
	"\n\nfindings must be present (an empty array is fine) with at most one entry per instance, per the instructions above."

// AmpAnalyzer implements Analyzer via a single bounded `amp -x` call,
// exactly mirroring ClaudeAnalyzer.Analyze's contract.
type AmpAnalyzer struct {
	Command ampexec.CommandFactory
	Config  ampexec.Config
}

func (a AmpAnalyzer) Analyze(ctx context.Context, snapshots []Snapshot) (Plan, error) {
	if a.Command == nil {
		return Plan{}, fmt.Errorf("no amp command factory configured")
	}
	payload, err := json.Marshal(struct {
		Task      string     `json:"task"`
		Snapshots []Snapshot `json:"snapshots"`
	}{
		Task:      "Assess these unhealthy agentmux sessions after their daily refresh and return a conservative repair plan grounded in the supplied issues.",
		Snapshots: snapshots,
	})
	if err != nil {
		return Plan{}, err
	}
	message := claudeSystemPrompt + "\n\n" + ampJSONInstructions + "\n\n" + string(payload)

	out, err := ampexec.Run(ctx, a.Command, a.Config, message)
	if err != nil {
		return Plan{}, fmt.Errorf("amp: %w", err)
	}
	plan, err := decodePlan(ampexec.ExtractJSON(out))
	if err != nil {
		return Plan{}, fmt.Errorf("reading amp result: %w", err)
	}
	return plan, nil
}
