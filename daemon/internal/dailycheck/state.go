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
	Fingerprint    string        `json:"fingerprint"`
	Issues         []HealthIssue `json:"issues,omitempty"`
	Problems       []string      `json:"problems,omitempty"`
	PendingMessage string        `json:"pending_message,omitempty"`
	UpdatedAt      time.Time     `json:"updated_at"`
}

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
