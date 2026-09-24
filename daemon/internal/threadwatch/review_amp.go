// The amp-backed nightly review escalation: ClaudeReviewer's twin
// (review_claude.go), driven through a single `amp -x` call instead of
// `claude -p --json-schema`. Same reviewSystemPrompt, same ReviewInput
// payload (buildReviewPayload), same ReviewResult parsing (decodeReview) —
// only the transport and the JSON-only instruction differ, because amp has
// no --json-schema flag of its own. See docs/thread-watch.md's "amp
// backend" section for the operator-facing picture and
// daemon/internal/ampexec for the shared mechanics/safety guarantees.
package threadwatch

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/m-rk/agentmux/daemon/internal/ampexec"
)

// reviewJSONInstructions spells out reviewSchema() (review_claude.go) for
// amp, in prose, since amp has no structured-output flag to enforce it.
// Keep this in lockstep with reviewSchema/decodeReview.
const reviewJSONInstructions = "Reply with ONLY a single JSON object matching this exact shape (a ```json code fence around it is fine; nothing else is):\n\n" +
	`{"summary": "string", "insights": [{"title": "string", "instances": ["string"], "evidence": "string", "suggestion": "string", "kind": "agents_md|permission|threshold|test|workflow|other"}]}` +
	"\n\ninsights has at most 5 items; an empty array is a good, expected result when nothing clears the bar described above."

// AmpReviewer turns a ReviewInput into a ReviewResult via a single bounded
// `amp -x` call, exactly mirroring ClaudeReviewer.Review's contract
// (including "a failure here means fall back to FallbackReview, not
// fatal").
type AmpReviewer struct {
	Command ampexec.CommandFactory
	Config  ampexec.Config
}

// Review implements the same contract as ClaudeReviewer.Review.
func (r AmpReviewer) Review(ctx context.Context, input ReviewInput) (ReviewResult, error) {
	if r.Command == nil {
		return ReviewResult{}, fmt.Errorf("no amp command factory configured")
	}
	payload, err := json.Marshal(buildReviewPayload(input))
	if err != nil {
		return ReviewResult{}, err
	}
	message := reviewSystemPrompt + "\n\n" + reviewJSONInstructions + "\n\n" + string(payload)

	out, err := ampexec.Run(ctx, r.Command, r.Config, message)
	if err != nil {
		return ReviewResult{}, fmt.Errorf("amp: %w", err)
	}
	result, err := decodeReview(ampexec.ExtractJSON(out))
	if err != nil {
		return ReviewResult{}, fmt.Errorf("reading amp result: %w", err)
	}
	return result, nil
}
