package main

import "testing"

func TestRefuseIfSelfTarget(t *testing.T) {
	cases := []struct {
		name     string
		host     string
		instance string
		force    bool
		tmux     string
		wantErr  bool
	}{
		{"not in tmux", "local", "agentmux-kilo", false, "", false},
		{"different socket", "local", "agentmux-kilo", false, "/tmp/tmux-1000/agentmux-family-llm-kilo,123,0", false},
		{"same socket", "local", "agentmux-kilo", false, "/tmp/tmux-1000/agentmux-agentmux-kilo,123,0", true},
		{"same socket but forced", "local", "agentmux-kilo", true, "/tmp/tmux-1000/agentmux-agentmux-kilo,123,0", false},
		{"remote host", "other-box", "agentmux-kilo", false, "/tmp/tmux-1000/agentmux-agentmux-kilo,123,0", false},
		{"blank host defaults to local", "", "agentmux-kilo", false, "/tmp/tmux-1000/agentmux-agentmux-kilo,123,0", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TMUX", tc.tmux)
			err := refuseIfSelfTarget(tc.host, tc.instance, tc.force)
			if tc.wantErr && err == nil {
				t.Fatal("want an error, got none")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want no error, got %v", err)
			}
		})
	}
}
