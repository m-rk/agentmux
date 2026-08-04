package dailycheck

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type NotificationState struct {
	Fingerprint    string                   `json:"fingerprint"`
	Issues         []HealthIssue            `json:"issues,omitempty"`
	Problems       []string                 `json:"problems,omitempty"`
	RepairAttempts map[string]RepairAttempt `json:"repair_attempts,omitempty"`
	PendingMessage string                   `json:"pending_message,omitempty"`
	UpdatedAt      time.Time                `json:"updated_at"`
}

type RepairAttempt struct {
	Fingerprint            string    `json:"fingerprint"`
	Action                 string    `json:"action"`
	ConsecutiveIneffective int       `json:"consecutive_ineffective"`
	LastAttemptAt          time.Time `json:"last_attempt_at"`
}

const MaxConsecutiveIneffectiveRepairs = 2

type NotificationDecision struct {
	Notify   bool
	Recovery bool
}

func LoadNotificationState(path string) (NotificationState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return NotificationState{}, nil
		}
		return NotificationState{}, err
	}
	var state NotificationState
	if err := json.Unmarshal(data, &state); err != nil {
		return NotificationState{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	return state, nil
}

func SaveNotificationState(path string, state NotificationState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	state.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".doctor-state-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func NotificationStateFor(report Report) NotificationState {
	return NotificationState{
		Fingerprint: issueFingerprint(report.AfterIssues, report.Problems),
		Issues:      append([]HealthIssue(nil), report.AfterIssues...),
		Problems:    append([]string(nil), report.Problems...),
	}
}

// AdvanceNotificationState carries forward repair history only while the same
// issue remains unresolved. Two consecutive attempts with an unchanged
// before/after fingerprint are enough to suppress that action on later runs;
// any recovery or materially different issue clears the guard automatically.
func AdvanceNotificationState(report Report, previous NotificationState) NotificationState {
	next := NotificationStateFor(report)
	next.PendingMessage = previous.PendingMessage
	next.RepairAttempts = map[string]RepairAttempt{}
	after := issuesByInstance(report.AfterIssues)
	before := issuesByInstance(report.BeforeIssues)

	for instance, attempt := range previous.RepairAttempts {
		if instanceIssueFingerprint(after[instance]) == attempt.Fingerprint {
			next.RepairAttempts[instance] = attempt
		}
	}
	for _, outcome := range report.Outcomes {
		if !outcome.Attempted || outcome.Action == "" || outcome.Action == ActionNone {
			continue
		}
		beforeFingerprint := instanceIssueFingerprint(before[outcome.Instance])
		afterFingerprint := instanceIssueFingerprint(after[outcome.Instance])
		if beforeFingerprint == "" || afterFingerprint != beforeFingerprint {
			delete(next.RepairAttempts, outcome.Instance)
			continue
		}
		count := 1
		if prior, ok := previous.RepairAttempts[outcome.Instance]; ok &&
			prior.Fingerprint == beforeFingerprint && prior.Action == outcome.Action {
			count = prior.ConsecutiveIneffective + 1
		}
		next.RepairAttempts[outcome.Instance] = RepairAttempt{
			Fingerprint:            beforeFingerprint,
			Action:                 outcome.Action,
			ConsecutiveIneffective: count,
			LastAttemptAt:          time.Now().UTC(),
		}
	}
	if len(next.RepairAttempts) == 0 {
		next.RepairAttempts = nil
	}
	return next
}

func repairSuppressed(snapshot Snapshot, finding Finding, attempts map[string]RepairAttempt) (RepairAttempt, bool) {
	attempt, ok := attempts[snapshot.Name]
	if !ok || attempt.Action != finding.Action || attempt.ConsecutiveIneffective < MaxConsecutiveIneffectiveRepairs {
		return RepairAttempt{}, false
	}
	return attempt, attempt.Fingerprint == instanceIssueFingerprint(snapshot.Issues)
}

func instanceIssueFingerprint(issues []HealthIssue) string {
	return issueFingerprint(issues, nil)
}

// DecideNotification debounces a problem that remains unchanged across daily
// runs, while still reporting a new incident, an auto-recovered recurrence,
// or recovery since the previous run.
func DecideNotification(report Report, previous NotificationState) NotificationDecision {
	current := NotificationStateFor(report)
	if len(report.BeforeIssues) > 0 || len(report.Problems) > 0 {
		if current.Fingerprint != "" && current.Fingerprint == previous.Fingerprint {
			return NotificationDecision{}
		}
		return NotificationDecision{Notify: true}
	}
	if current.Fingerprint == "" && previous.Fingerprint != "" {
		return NotificationDecision{Notify: true, Recovery: true}
	}
	return NotificationDecision{}
}

func FormatRecoveryNotification(host string, previous NotificationState) string {
	title := "✅ agentmux doctor"
	if host != "" {
		title += " on " + cleanOneLine(host)
	}
	lines := []string{title, "Previously reported session problems are no longer present."}
	byInstance := issuesByInstance(previous.Issues)
	for _, instance := range sortedIssueInstances(byInstance, map[string][]HealthIssue{}, map[string]Outcome{}) {
		issues := byInstance[instance]
		lines = append(lines, "• "+instance+": recovered from "+issueSummaries(issues))
	}
	for _, problem := range previous.Problems {
		lines = append(lines, "• recovered from: "+cleanOneLine(problem))
	}
	message := strings.ReplaceAll(strings.Join(lines, "\n"), "@", "@\u200b")
	if len(message) > 1900 {
		message = message[:1900] + "…"
	}
	return message
}
