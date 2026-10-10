package ops

import (
	"context"
	"testing"
)

func TestRemoveArchivesInstanceThroughDaemon(t *testing.T) {
	d := &fakeDaemon{}
	res, err := (Env{Dial: func() (Daemon, error) { return d, nil }}).Remove(context.Background(), RemoveRequest{Instance: "orchestrator"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Instance != "orchestrator" || res.Message != "retired orchestrator" {
		t.Fatalf("result = %+v", res)
	}
	if len(d.retired) != 1 || d.retired[0].Instance != "orchestrator" || !d.retired[0].Archive {
		t.Fatalf("managed request = %+v", d.retired)
	}
}

func TestRemoveRejectsInvalidInstanceWithoutDialing(t *testing.T) {
	dialed := false
	_, err := (Env{Dial: func() (Daemon, error) { dialed = true; return &fakeDaemon{}, nil }}).Remove(context.Background(), RemoveRequest{Instance: "../bad"})
	if err == nil {
		t.Fatal("invalid instance was accepted")
	}
	if dialed {
		t.Fatal("invalid name reached daemon")
	}
}
