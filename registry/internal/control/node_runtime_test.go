package control

import (
	"errors"
	"testing"
)

func TestNodeRuntimeAdTagLifecycle(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "runtime-user", 5)
	const nodeID = "runtime-aaaaa"
	if err := s.AuthorizeNode(nodeID, u); err != nil {
		t.Fatal(err)
	}
	rt, err := s.RuntimeSettingsForNode(nodeID)
	if err != nil || rt.State != RuntimeUnmanaged || rt.AdTagMode != AdTagUnmanaged {
		t.Fatalf("default runtime: %+v err=%v", rt, err)
	}
	rt, err = s.SetNodeAdTag(u.ID, nodeID, AdTagSet, "0123456789ABCDEF0123456789abcdef", 0)
	if err != nil || rt.State != RuntimePending || rt.DesiredRevision != 1 || rt.AdTagValue != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("set ad_tag: %+v err=%v", rt, err)
	}
	if _, err := s.SetNodeAdTag(u.ID, nodeID, AdTagSet, "not-hex", 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid tag must be rejected, got %v", err)
	}
	rt, err = s.ReportNodeRuntime(nodeID, 1, 1, RuntimeApplied, "")
	if err != nil || rt.State != RuntimeApplied || rt.AppliedRevision != 1 {
		t.Fatalf("report applied: %+v err=%v", rt, err)
	}
	// A stale ack must not overwrite a newer desired revision.
	rt, err = s.SetNodeAdTag(u.ID, nodeID, AdTagSet, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", rt.DesiredRevision)
	if err != nil || rt.DesiredRevision != 2 {
		t.Fatalf("second set: %+v err=%v", rt, err)
	}
	rt, err = s.ReportNodeRuntime(nodeID, 1, 1, RuntimeApplied, "")
	if err != nil || rt.State != RuntimePending || rt.DesiredRevision != 2 {
		t.Fatalf("stale ack must keep pending: %+v err=%v", rt, err)
	}
	// Foreign user cannot set.
	other := mustUser(t, s, "runtime-other", 5)
	if _, err := s.SetNodeAdTag(other.ID, nodeID, AdTagSet, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign set: want ErrNotFound, got %v", err)
	}
}
