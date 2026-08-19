package wizardui

import "testing"

func TestCapabilitiesForAgent(t *testing.T) {
	tests := []struct {
		agent string
		want  Capabilities
	}{
		{"claude-code", Capabilities{DisplayHostName: true, Compact: true}},
		{"zero", Capabilities{Provider: true}},
		{"opencode", Capabilities{Provider: true}},
		{"kilo", Capabilities{DisplayHostName: true, Provider: true, ProviderAPIKey: true}},
	}
	for _, tc := range tests {
		t.Run(tc.agent, func(t *testing.T) {
			if got := CapabilitiesForAgent(tc.agent); got != tc.want {
				t.Fatalf("CapabilitiesForAgent(%q) = %+v, want %+v", tc.agent, got, tc.want)
			}
		})
	}
}

func TestDefaultInstance(t *testing.T) {
	if got := DefaultInstance("claude-code"); got != "claude-code" {
		t.Fatalf("DefaultInstance(claude-code) = %q, want claude-code", got)
	}
	for _, agent := range []string{"zero", "opencode", "kilo"} {
		if got := DefaultInstance(agent); got != "agentmux" {
			t.Errorf("DefaultInstance(%s) = %q, want agentmux", agent, got)
		}
	}
}

func TestUsesDefaultProvider(t *testing.T) {
	for _, provider := range []string{"", "ollama", "  ollama  "} {
		if !UsesDefaultProvider(provider) {
			t.Errorf("UsesDefaultProvider(%q) = false, want true", provider)
		}
	}
	if UsesDefaultProvider("gateway") {
		t.Error("UsesDefaultProvider(gateway) = true, want false")
	}
}
