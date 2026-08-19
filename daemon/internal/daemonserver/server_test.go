package daemonserver

import (
	"context"
	"testing"

	"github.com/m-rk/agentmux/daemon/internal/pb"
	"github.com/m-rk/agentmux/daemon/internal/provision"
)

func TestGetCreateOptionsReturnsTargetHostDefault(t *testing.T) {
	resp, err := New().GetCreateOptions(context.Background(), &pb.GetCreateOptionsRequest{})
	if err != nil {
		t.Fatalf("GetCreateOptions: %v", err)
	}
	if want := provision.DefaultHostName(); resp.DefaultHostName != want {
		t.Fatalf("default host name = %q, want %q", resp.DefaultHostName, want)
	}
}
