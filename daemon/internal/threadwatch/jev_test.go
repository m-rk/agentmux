package threadwatch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/typesafe"
)

func sampleSignal() Signal {
	return Signal{
		Time:     time.Now(),
		Instance: "worker-1",
		Thread:   "thread-abc",
		Code:     CodeAwaitingUser,
		Tier:     TierIntervene,
		Reason:   "idle 12m on what looks like a question",
	}
}

func sampleEvents() []Event {
	now := time.Now()
	return []Event{
		{Time: now.Add(-5 * time.Minute), Instance: "worker-1", Agent: "claude-code", Kind: KindActivity, Tool: "bash"},
		{Time: now.Add(-2 * time.Minute), Instance: "worker-1", Agent: "claude-code", Kind: KindAssistantMsg, Excerpt: "Should I also migrate the DB schema, or just the data?"},
		{Time: now.Add(-1 * time.Minute), Instance: "worker-1", Agent: "claude-code", Kind: KindStatus, Status: "idle"},
	}
}

// jevResponse builds a canned systemone-shaped response containing an
// answer for every question id present in the request, so the same helper
// serves both the full Judge() question set and JudgeInsight()'s smaller
// one.
func jevResponse(questionIDs []string) map[string]any {
	answers := map[string]any{}
	for _, id := range questionIDs {
		switch id {
		case "waiting_kind":
			answers[id] = map[string]any{
				"type":          "choice",
				"choice":        "question_to_user",
				"confidence":    0.82,
				"probabilities": map[string]float64{"question_to_user": 0.82, "none": 0.1, "finished": 0.08},
			}
		case "needs_human_now":
			answers[id] = map[string]any{"type": "noul", "noul": 0.91}
		case "urgency":
			answers[id] = map[string]any{
				"type":       "score",
				"score":      3.4,
				"confidence": 0.7,
				"legend":     map[string]string{"0": "nothing", "4": "blocked"},
			}
		case "likely_transient":
			answers[id] = map[string]any{"type": "noul", "noul": 0.2}
		case "category":
			answers[id] = map[string]any{"type": "choice", "choice": "unclear_task", "confidence": 0.6, "probabilities": map[string]float64{"unclear_task": 0.6}}
		case "fixable_by_config":
			answers[id] = map[string]any{"type": "noul", "noul": 0.3}
		}
	}
	return map[string]any{
		"model":   "jev-1.13.0",
		"answers": answers,
		"usage":   map[string]any{"input_tokens": 100, "output_tokens": 20},
	}
}

func TestJevJudge_Judge_RequestShapeAndParsing(t *testing.T) {
	var requests int32
	var gotAuth, gotModel string
	var gotQuestionIDs []string
	var gotState map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		gotAuth = r.Header.Get("Authorization")

		var body struct {
			State     map[string]any             `json:"state"`
			Model     string                     `json:"model"`
			Questions map[string]json.RawMessage `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		gotModel = body.Model
		gotState = body.State
		for id := range body.Questions {
			gotQuestionIDs = append(gotQuestionIDs, id)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(jevResponse(gotQuestionIDs))
	}))
	defer srv.Close()

	judge := JevJudge{Client: &typesafe.Client{BaseURL: srv.URL, APIKey: "the-real-key"}, Model: "jev-latest"}
	jm := judge.Judge(context.Background(), sampleSignal(), sampleEvents())

	if requests != 1 {
		t.Fatalf("requests = %d, want 1 (all questions must go in one call)", requests)
	}
	if gotAuth != "Bearer the-real-key" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotModel != "jev-latest" {
		t.Errorf("model = %q", gotModel)
	}

	wantIDs := map[string]bool{"urgency": true, "needs_human_now": true, "category": true, "fixable_by_config": true, "waiting_kind": true}
	if len(gotQuestionIDs) != len(wantIDs) {
		t.Errorf("question ids = %v, want %v", gotQuestionIDs, wantIDs)
	}
	for _, id := range gotQuestionIDs {
		if !wantIDs[id] {
			t.Errorf("unexpected question id %q", id)
		}
	}
	for id := range wantIDs {
		found := false
		for _, got := range gotQuestionIDs {
			if got == id {
				found = true
			}
		}
		if !found {
			t.Errorf("missing question id %q", id)
		}
	}
	// awaiting_user is not error_loop/auth_failed, so likely_transient must
	// not be asked.
	for _, id := range gotQuestionIDs {
		if id == "likely_transient" {
			t.Errorf("likely_transient asked for %s, should only be asked for error_loop/auth_failed", CodeAwaitingUser)
		}
	}

	// State shape: instance, signal.code/reason, last_assistant_message,
	// recent_events.
	if gotState["instance"] != "worker-1" {
		t.Errorf("state.instance = %v", gotState["instance"])
	}
	sig, ok := gotState["signal"].(map[string]any)
	if !ok || sig["code"] != CodeAwaitingUser {
		t.Errorf("state.signal = %v", gotState["signal"])
	}
	if lam, _ := gotState["last_assistant_message"].(string); !strings.Contains(lam, "migrate the DB") {
		t.Errorf("state.last_assistant_message = %q", lam)
	}
	events, ok := gotState["recent_events"].([]any)
	if !ok || len(events) != 3 {
		t.Errorf("state.recent_events = %v", gotState["recent_events"])
	}

	// Response parsed into Judgment.
	if jm.Err != "" {
		t.Fatalf("Judgment.Err = %q, want empty", jm.Err)
	}
	if jm.WaitingKind != "question_to_user" {
		t.Errorf("WaitingKind = %q", jm.WaitingKind)
	}
	if jm.WaitingKindProbs["question_to_user"] != 0.82 {
		t.Errorf("WaitingKindProbs = %v", jm.WaitingKindProbs)
	}
	if jm.NeedsHumanNow != 0.91 {
		t.Errorf("NeedsHumanNow = %v", jm.NeedsHumanNow)
	}
	if jm.Urgency != 3.4 || jm.UrgencyConf != 0.7 {
		t.Errorf("Urgency = %v, UrgencyConf = %v", jm.Urgency, jm.UrgencyConf)
	}
	if jm.Category != "unclear_task" {
		t.Errorf("Category = %q", jm.Category)
	}
	if jm.FixableByConfig != 0.3 {
		t.Errorf("FixableByConfig = %v", jm.FixableByConfig)
	}
	if jm.LikelyTransient != 0 {
		t.Errorf("LikelyTransient = %v, want 0 (not asked)", jm.LikelyTransient)
	}
}

func TestJevJudge_Judge_ErrorLoopAsksLikelyTransientNotWaitingKind(t *testing.T) {
	var gotQuestionIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Questions map[string]json.RawMessage `json:"questions"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		gotQuestionIDs = nil
		for id := range body.Questions {
			gotQuestionIDs = append(gotQuestionIDs, id)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(jevResponse(gotQuestionIDs))
	}))
	defer srv.Close()

	sig := sampleSignal()
	sig.Code = CodeErrorLoop
	judge := JevJudge{Client: &typesafe.Client{BaseURL: srv.URL, APIKey: "k"}}
	jm := judge.Judge(context.Background(), sig, sampleEvents())

	if jm.Err != "" {
		t.Fatalf("Judgment.Err = %q", jm.Err)
	}
	hasWaitingKind, hasLikelyTransient := false, false
	for _, id := range gotQuestionIDs {
		if id == "waiting_kind" {
			hasWaitingKind = true
		}
		if id == "likely_transient" {
			hasLikelyTransient = true
		}
	}
	if hasWaitingKind {
		t.Error("waiting_kind asked for error_loop, want only for awaiting_user/stalled_turn")
	}
	if !hasLikelyTransient {
		t.Error("likely_transient not asked for error_loop")
	}
	if jm.LikelyTransient != 0.2 {
		t.Errorf("LikelyTransient = %v", jm.LikelyTransient)
	}
}

func TestJevJudge_JudgeInsight_OnlyTwoQuestions(t *testing.T) {
	var gotQuestionIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Questions map[string]json.RawMessage `json:"questions"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		for id := range body.Questions {
			gotQuestionIDs = append(gotQuestionIDs, id)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(jevResponse(gotQuestionIDs))
	}))
	defer srv.Close()

	sig := sampleSignal()
	sig.Code = CodeSlowTurn
	sig.Tier = TierInsight
	judge := JevJudge{Client: &typesafe.Client{BaseURL: srv.URL, APIKey: "k"}}
	jm := judge.JudgeInsight(context.Background(), sig, sampleEvents())

	if len(gotQuestionIDs) != 2 {
		t.Fatalf("question ids = %v, want exactly [category fixable_by_config]", gotQuestionIDs)
	}
	if jm.Category != "unclear_task" || jm.FixableByConfig != 0.3 {
		t.Errorf("jm = %+v", jm)
	}
	if jm.WaitingKind != "" || jm.Urgency != 0 || jm.NeedsHumanNow != 0 {
		t.Errorf("JudgeInsight populated fields it did not ask for: %+v", jm)
	}
}

func TestJevJudge_Judge_RetriesOn529ThenSucceeds(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			w.WriteHeader(529)
			w.Write([]byte("overloaded"))
			return
		}
		var body struct {
			Questions map[string]json.RawMessage `json:"questions"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		var ids []string
		for id := range body.Questions {
			ids = append(ids, id)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(jevResponse(ids))
	}))
	defer srv.Close()

	judge := JevJudge{Client: &typesafe.Client{BaseURL: srv.URL, APIKey: "k", MaxRetries: 2}}
	jm := judge.Judge(context.Background(), sampleSignal(), sampleEvents())

	if jm.Err != "" {
		t.Fatalf("Judgment.Err = %q, want empty after retry succeeds", jm.Err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}
}

func TestJevJudge_Judge_401SetsErrWithoutKey(t *testing.T) {
	const key = "top-secret-typesafe-key"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"Missing or invalid API key"}`))
	}))
	defer srv.Close()

	judge := JevJudge{Client: &typesafe.Client{BaseURL: srv.URL, APIKey: key}}
	jm := judge.Judge(context.Background(), sampleSignal(), sampleEvents())

	if jm.Err == "" {
		t.Fatal("Judgment.Err = \"\", want a 401 error message")
	}
	if strings.Contains(jm.Err, key) {
		t.Fatalf("Judgment.Err leaks API key: %q", jm.Err)
	}
	// Every other field must stay zero on failure.
	if jm.Urgency != 0 || jm.NeedsHumanNow != 0 || jm.WaitingKind != "" || jm.Category != "" {
		t.Errorf("non-Err fields set on failure: %+v", jm)
	}
}

func TestJevJudge_Judge_ContextCancelled(t *testing.T) {
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	judge := JevJudge{Client: &typesafe.Client{BaseURL: srv.URL, APIKey: "k"}}
	jm := judge.Judge(ctx, sampleSignal(), sampleEvents())

	if jm.Err == "" {
		t.Fatal("Judgment.Err = \"\", want a context-cancelled error")
	}
	if requests != 0 {
		t.Errorf("requests = %d, want 0 (cancelled ctx must not reach the server)", requests)
	}
}

func TestJevJudge_Judge_NoClient(t *testing.T) {
	judge := JevJudge{}
	jm := judge.Judge(context.Background(), sampleSignal(), sampleEvents())
	if jm.Err == "" {
		t.Fatal("Judgment.Err = \"\", want error for missing client")
	}
}
