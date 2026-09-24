package typesafe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewFromEnv(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "")
	if _, ok := NewFromEnv(); ok {
		t.Fatal("NewFromEnv() ok=true with empty key")
	}

	t.Setenv("TYPESAFE_API_KEY", "sk-test-123")
	c, ok := NewFromEnv()
	if !ok {
		t.Fatal("NewFromEnv() ok=false with key set")
	}
	if c.APIKey != "sk-test-123" {
		t.Fatalf("APIKey = %q", c.APIKey)
	}
}

func TestAsk_RequestShapeAndResponseParsing(t *testing.T) {
	var gotAuth, gotContentType, gotPath, gotMethod string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"model": "jev-1.13.0",
			"answers": map[string]any{
				"is_ready": map[string]any{"type": "noul", "noul": 0.87},
				"team": map[string]any{
					"type":          "choice",
					"choice":        "billing",
					"confidence":    0.9,
					"probabilities": map[string]float64{"billing": 0.9, "shipping": 0.1},
				},
			},
			"usage": map[string]any{"input_tokens": 42, "output_tokens": 7},
		})
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "test-key-abc"}
	questions := map[string]Question{
		"is_ready": {Type: "noul", Instructions: "Is the order ready?"},
		"team":     {Type: "choice", Instructions: "Which team?", Criteria: map[string]string{"billing": "billing issues", "shipping": "shipping issues"}},
	}
	answers, usage, err := c.Ask(context.Background(), map[string]string{"order": "abc"}, "jev-latest", questions)
	if err != nil {
		t.Fatalf("Ask() error = %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != systemOnePath {
		t.Errorf("path = %q, want %q", gotPath, systemOnePath)
	}
	if gotAuth != "Bearer test-key-abc" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if !strings.HasPrefix(gotContentType, "application/json") {
		t.Errorf("Content-Type = %q", gotContentType)
	}
	if gotBody["model"] != "jev-latest" {
		t.Errorf("body model = %v", gotBody["model"])
	}
	if _, ok := gotBody["questions"].(map[string]any)["is_ready"]; !ok {
		t.Errorf("body missing question is_ready: %v", gotBody["questions"])
	}
	if _, ok := gotBody["questions"].(map[string]any)["team"]; !ok {
		t.Errorf("body missing question team: %v", gotBody["questions"])
	}

	if usage.InputTokens != 42 || usage.OutputTokens != 7 {
		t.Errorf("usage = %+v", usage)
	}
	if got := answers["is_ready"].Noul; got != 0.87 {
		t.Errorf("is_ready.Noul = %v", got)
	}
	if got := answers["team"].Choice; got != "billing" {
		t.Errorf("team.Choice = %v", got)
	}
	if got := answers["team"].Probabilities["shipping"]; got != 0.1 {
		t.Errorf("team.Probabilities[shipping] = %v", got)
	}
}

func TestAsk_RetriesOn529ThenSucceeds(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			w.WriteHeader(529)
			w.Write([]byte(`{"error":"overloaded"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"model":   "jev-1.13.0",
			"answers": map[string]any{"q": map[string]any{"type": "noul", "noul": 0.5}},
			"usage":   map[string]any{"input_tokens": 1, "output_tokens": 1},
		})
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "k", MaxRetries: 2}
	answers, _, err := c.Ask(context.Background(), "state", "jev-latest", map[string]Question{"q": {Type: "noul", Instructions: "?"}})
	if err != nil {
		t.Fatalf("Ask() error = %v", err)
	}
	if answers["q"].Noul != 0.5 {
		t.Errorf("answers[q].Noul = %v", answers["q"].Noul)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
}

func TestAsk_RetriesOn429ThenExhausts(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte("rate limited"))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "k", MaxRetries: 1}
	_, _, err := c.Ask(context.Background(), "state", "jev-latest", map[string]Question{"q": {Type: "noul", Instructions: "?"}})
	if err == nil {
		t.Fatal("Ask() error = nil, want rate-limit error")
	}
	if got := atomic.LoadInt32(&attempts); got != 2 { // initial + 1 retry
		t.Errorf("attempts = %d, want 2", got)
	}
}

func TestAsk_401DoesNotRetryAndOmitsKey(t *testing.T) {
	var attempts int32
	const key = "super-secret-key-xyz"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"Missing or invalid API key"}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: key, MaxRetries: 3}
	_, _, err := c.Ask(context.Background(), "state", "jev-latest", map[string]Question{"q": {Type: "noul", Instructions: "?"}})
	if err == nil {
		t.Fatal("Ask() error = nil, want 401 error")
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("error contains API key: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1 (401 must not retry)", got)
	}
}

func TestAsk_ContextCancelled(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := &Client{BaseURL: srv.URL, APIKey: "k"}
	_, _, err := c.Ask(ctx, "state", "jev-latest", map[string]Question{"q": {Type: "noul", Instructions: "?"}})
	if err == nil {
		t.Fatal("Ask() error = nil, want context error")
	}
	if got := atomic.LoadInt32(&attempts); got != 0 {
		t.Errorf("attempts = %d, want 0 (request must not be sent with a cancelled ctx)", got)
	}
}

func TestAsk_NoAPIKey(t *testing.T) {
	c := &Client{}
	_, _, err := c.Ask(context.Background(), "state", "jev-latest", nil)
	if err == nil {
		t.Fatal("Ask() error = nil, want error for missing API key")
	}
}

func TestBackoffBounded(t *testing.T) {
	for attempt := 1; attempt <= 10; attempt++ {
		d := backoff(attempt)
		if d <= 0 || d > 5*time.Second {
			t.Errorf("backoff(%d) = %v, want (0, 5s]", attempt, d)
		}
	}
}
