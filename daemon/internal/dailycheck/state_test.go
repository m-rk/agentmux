package dailycheck

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestNotificationDecisionDebouncesPersistentIssue(t *testing.T) {
	issue := HealthIssue{Instance: "one", Code: "remote-disconnected", Summary: "remote is disconnected"}
	report := Report{BeforeIssues: []HealthIssue{issue}, AfterIssues: []HealthIssue{issue}}
	previous := NotificationStateFor(report)
	if decision := DecideNotification(report, previous); decision.Notify {
		t.Fatalf("persistent unchanged issue should be debounced: %+v", decision)
	}
}

func TestNotificationDecisionReportsAutoRecoveryAndLaterRecovery(t *testing.T) {
	issue := HealthIssue{Instance: "one", Code: "session-dead", Summary: "session is not running"}
	autoRecovered := Report{BeforeIssues: []HealthIssue{issue}}
	if decision := DecideNotification(autoRecovered, NotificationState{}); !decision.Notify || decision.Recovery {
		t.Fatalf("auto-recovery decision = %+v", decision)
	}
	persistent := NotificationStateFor(Report{AfterIssues: []HealthIssue{issue}})
	if decision := DecideNotification(Report{}, persistent); !decision.Notify || !decision.Recovery {
		t.Fatalf("later recovery decision = %+v", decision)
	}
}

func TestNotificationDecisionTreatsDoctorFailureAsIncident(t *testing.T) {
	previous := NotificationStateFor(Report{AfterIssues: []HealthIssue{{Instance: "one", Code: "session-dead"}}})
	report := Report{Problems: []string{"doctor run failed: daemon unavailable"}}
	decision := DecideNotification(report, previous)
	if !decision.Notify || decision.Recovery {
		t.Fatalf("doctor failure decision = %+v", decision)
	}
}

func TestNotificationStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "doctor.json")
	want := NotificationState{
		Fingerprint:    "one:dead",
		Issues:         []HealthIssue{{Instance: "one", Code: "dead"}},
		RepairAttempts: map[string]RepairAttempt{"one": {Fingerprint: "one:dead", Action: ActionStart, ConsecutiveIneffective: 2}},
		PendingMessage: "retry me",
	}
	if err := SaveNotificationState(path, want); err != nil {
		t.Fatalf("SaveNotificationState: %v", err)
	}
	got, err := LoadNotificationState(path)
	if err != nil {
		t.Fatalf("LoadNotificationState: %v", err)
	}
	if got.Fingerprint != want.Fingerprint || len(got.Issues) != 1 || got.PendingMessage != want.PendingMessage || got.RepairAttempts["one"].ConsecutiveIneffective != 2 {
		t.Fatalf("state = %+v", got)
	}
}

func TestAdvanceNotificationStateTracksAndClearsIneffectiveRepairs(t *testing.T) {
	issue := HealthIssue{Instance: "one", Code: "remote-disconnected"}
	report := Report{
		BeforeIssues: []HealthIssue{issue},
		AfterIssues:  []HealthIssue{issue},
		Outcomes:     []Outcome{{Instance: "one", Action: ActionRestart, Attempted: true}},
	}
	first := AdvanceNotificationState(report, NotificationState{})
	if got := first.RepairAttempts["one"].ConsecutiveIneffective; got != 1 {
		t.Fatalf("first ineffective count = %d", got)
	}
	second := AdvanceNotificationState(report, first)
	if got := second.RepairAttempts["one"].ConsecutiveIneffective; got != 2 {
		t.Fatalf("second ineffective count = %d", got)
	}
	recovered := AdvanceNotificationState(Report{}, second)
	if len(recovered.RepairAttempts) != 0 {
		t.Fatalf("repair attempts survived recovery: %+v", recovered.RepairAttempts)
	}
}

func TestFormatRecoveryNotificationIsStable(t *testing.T) {
	state := NotificationState{Issues: []HealthIssue{
		{Instance: "zeta", Summary: "dead"},
		{Instance: "alpha", Summary: "disconnected"},
	}}
	message := FormatRecoveryNotification("host", state)
	if strings.Index(message, "alpha") > strings.Index(message, "zeta") {
		t.Fatalf("recovery instances are not sorted: %q", message)
	}
}
