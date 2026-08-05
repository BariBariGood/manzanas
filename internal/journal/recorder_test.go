package journal

import (
	"context"
	"testing"

	"github.com/BariBariGood/manzanas/proto"
)

func TestNilRecorderIsNoop(t *testing.T) {
	var r *Recorder
	if got := r.Record(context.Background(), Event{LeaseID: "x", Action: "a"}); got != (proto.JournalRef{}) {
		t.Fatalf("ref = %+v", got)
	}
	if r.Store() != nil {
		t.Fatal("Store() != nil")
	}
	r.StartRun(proto.Lease{ID: "x"}, nil, "v0") // must not panic
	if NewRecorder(nil) != nil {
		t.Fatal("NewRecorder(nil) != nil")
	}
}

func TestRecordBuildsPayload(t *testing.T) {
	s := testStore(t)
	r := NewRecorder(s)
	ctx := context.Background()
	ref := r.Record(ctx, Event{
		LeaseID: "run1", AgentID: "agent-1", Action: "targets.boot",
		Params: map[string]any{"udid": "U1"}, Status: "ok",
		AXBefore: "hashA", AXAfter: "hashB",
		Artifacts: []ArtifactRef{{Path: "artifacts/ab.png", SHA256: "ab", Bytes: 2}},
		Extra:     map[string]any{"note": "hi"},
	})
	if ref.Seq != 1 {
		t.Fatalf("ref = %+v", ref)
	}
	entries, err := s.Read(ctx, "run1", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	p := entries[0].Payload
	for k, want := range map[string]any{
		"lease_id": "run1", "agent_id": "agent-1", "action": "targets.boot",
		"status": "ok", "ax_before": "hashA", "ax_after": "hashB", "note": "hi",
	} {
		if p[k] != want {
			t.Errorf("payload[%q] = %v, want %v", k, p[k], want)
		}
	}
	if entries[0].Kind != "action" {
		t.Errorf("kind = %q", entries[0].Kind)
	}
	if _, ok := p["artifacts"].([]any); !ok {
		t.Errorf("artifacts missing: %v", p["artifacts"])
	}
}

func TestStartRunWritesMeta(t *testing.T) {
	s := testStore(t)
	r := NewRecorder(s)
	target := proto.Target{UDID: "U1", Name: "iPhone 17 Pro", Runtime: "iOS 26.5", DeviceType: "iPhone 17 Pro"}
	r.StartRun(proto.Lease{ID: "run1", AgentID: "a1", Purpose: "test"}, &target, "v0")
	meta, err := s.ReadMeta("run1")
	if err != nil {
		t.Fatal(err)
	}
	if meta.TargetUDID != "U1" || meta.AgentID != "a1" || meta.FormatVersion != FormatVersion {
		t.Fatalf("meta = %+v", meta)
	}
}
