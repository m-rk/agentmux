package modelsource

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadRegistryAndRejectPlainCredential(t *testing.T) {
	p := filepath.Join(t.TempDir(), "models.yaml")
	if err := os.WriteFile(p, []byte("sources:\n  - name: laptop\n    kind: ollama-local\n    endpoint: http://127.0.0.1:11434\n    cost: free\n    telemetry: false\n    training: false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := Load(p)
	if err != nil || len(r.Sources) != 1 || r.Sources[0].Name != "laptop" {
		t.Fatalf("Load() = %#v, %v", r, err)
	}
	if err := os.WriteFile(p, []byte("sources:\n  - name: laptop\n    kind: openai-compatible\n    endpoint: https://example.invalid\n    credential_ref: secret-value\n    cost: metered\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("expected plaintext credential reference to be rejected")
	}
}

func TestExpiry(t *testing.T) {
	results := Check(t.Context(), []Source{{Name: "old", Kind: "ollama-local", Endpoint: "http://example.invalid", Cost: "free", Expires: "2020-01-01"}}, "/does/not/matter", time.Now())
	if len(results) != 1 || results[0].Reachable || results[0].Reason != "expired" {
		t.Fatalf("expired result = %#v", results)
	}
}
