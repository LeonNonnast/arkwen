package conformance

import (
	"encoding/json"
	"testing"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"github.com/arkwen/arkwen/internal/eventlog"
)

// goldenStream loads the golden-run vector's stream into typed envelopes.
func goldenStream(t *testing.T) []*arkwenv1.EventEnvelope {
	t.Helper()
	v := loadVectorRaw(t, "golden-run.json")
	var given struct {
		Stream []json.RawMessage `json:"stream"`
	}
	if err := json.Unmarshal(v["given"], &given); err != nil {
		t.Fatalf("decode given: %v", err)
	}
	out := make([]*arkwenv1.EventEnvelope, 0, len(given.Stream))
	for _, raw := range given.Stream {
		out = append(out, unmarshalEnvelope(t, raw))
	}
	return out
}

// G1: seq strictly increasing, unique, starts at 0, contiguous 0..9; UNIQUE(runId,seq).
func TestG1_SeqMonotoneUnique(t *testing.T) {
	stream := goldenStream(t)
	seen := map[uint64]bool{}
	for i, e := range stream {
		if e.GetSeq() != uint64(i) {
			t.Fatalf("seq not contiguous at index %d: got %d", i, e.GetSeq())
		}
		if seen[e.GetSeq()] {
			t.Fatalf("duplicate seq %d", e.GetSeq())
		}
		seen[e.GetSeq()] = true
	}
	if len(stream) != 10 {
		t.Fatalf("want 10 events, got %d", len(stream))
	}
}

// G2: no mutate event types exist in the taxonomy; exactly one RUN_FINISHED, last.
func TestG2_NoMutateTypes_OneTerminalLast(t *testing.T) {
	// structural: the EventType enum has no update/delete/edit/tombstone member
	ed := arkwenv1.EventType(0).Descriptor()
	for i := 0; i < ed.Values().Len(); i++ {
		name := string(ed.Values().Get(i).Name())
		for _, bad := range []string{"UPDATE", "DELETE", "EDIT", "TOMBSTONE", "REVISION"} {
			if contains(name, bad) {
				t.Fatalf("taxonomy contains mutate type %s", name)
			}
		}
	}
	stream := goldenStream(t)
	finishedCount := 0
	for i, e := range stream {
		if e.GetType() == arkwenv1.EventType_EVENT_TYPE_RUN_FINISHED {
			finishedCount++
			if i != len(stream)-1 {
				t.Fatalf("run.finished must be last, found at %d", i)
			}
		}
	}
	if finishedCount != 1 {
		t.Fatalf("want exactly one run.finished, got %d", finishedCount)
	}
}

// G3: every envelope is structurally valid and its type matches its payload.
func TestG3_EnvelopeValidity(t *testing.T) {
	for _, e := range goldenStream(t) {
		if e.GetRunId() != "run-golden-0001" {
			t.Fatalf("bad runId %q", e.GetRunId())
		}
		if err := eventlog.Validate(e); err != nil {
			t.Fatalf("envelope seq %d invalid: %v", e.GetSeq(), err)
		}
	}
}

// G4: seed frozen at run.created; never re-carried.
func TestG4_SeedFrozenAtCreated(t *testing.T) {
	stream := goldenStream(t)
	if stream[0].GetType() != arkwenv1.EventType_EVENT_TYPE_RUN_CREATED {
		t.Fatal("stream[0] must be run.created")
	}
	seed := stream[0].GetRunCreated().GetSeed()
	checks := map[string]bool{
		"missionHash":          seed.GetMissionHash() != nil,
		"imageDigest":          seed.GetImageDigest() != nil,
		"toolkitVersions":      len(seed.GetToolkitVersions()) > 0,
		"workbenchSnapshotRef": seed.GetWorkbenchSnapshotRef() != nil,
		"adapterVersion":       seed.GetAdapterVersion() != "",
		"policyVersion":        seed.GetPolicyVersion() != nil,
		"sessionSeed":          seed.GetSessionSeed() != "",
		"blueprintDigest":      seed.GetBlueprintDigest() != nil,
		"isolationContractRef": seed.GetIsolationContractRef() != nil,
		"tenantId":             seed.GetTenantId() != "",
		"authzPolicyVersion":   seed.GetAuthzPolicyVersion() != nil,
		"egressPolicyHash":     seed.GetEgressPolicyHash() != nil,
		"resourceLimitsHash":   seed.GetResourceLimitsHash() != nil,
		"imageSignatureRef":    seed.GetImageSignatureRef() != nil,
		"imageProvenanceRef":   seed.GetImageProvenanceRef() != nil,
		"secretScopeSetRef":    seed.GetSecretScopeSetRef() != nil,
	}
	for name, ok := range checks {
		if !ok {
			t.Errorf("seed field %s not populated", name)
		}
	}
	for _, e := range stream[1:] {
		if e.GetRunCreated() != nil {
			t.Fatalf("seed re-carried at seq %d", e.GetSeq())
		}
	}
}

// G5: content-bearing fields are ContentRef pointers; no bytes content field exists.
func TestG5_PointerOnly(t *testing.T) {
	if bf := bytesContentFields(); len(bf) != 0 {
		t.Fatalf("bytes content field(s) present (Invariant 4 violated): %v", bf)
	}
	for _, e := range goldenStream(t) {
		var ref *arkwenv1.ContentRef
		switch p := e.GetPayload().(type) {
		case *arkwenv1.EventEnvelope_WorkerMessage:
			ref = p.WorkerMessage.GetMessageRef()
		case *arkwenv1.EventEnvelope_WorkerToolCall:
			ref = p.WorkerToolCall.GetArgumentsRef()
		case *arkwenv1.EventEnvelope_WorkerToolResult:
			ref = p.WorkerToolResult.GetResultRef()
		case *arkwenv1.EventEnvelope_WorkerArtifactWritten:
			ref = p.WorkerArtifactWritten.GetArtifact()
		case *arkwenv1.EventEnvelope_WorkerRaw:
			ref = p.WorkerRaw.GetRawRef()
		default:
			continue
		}
		if ref.GetContentHash() == nil || ref.GetContentHash().GetHex() == "" {
			t.Fatalf("content field at seq %d lacks a content hash", e.GetSeq())
		}
	}
}

// G6: paused is never a lifecycle state; the terminal is a legal terminal.
func TestG6_NoPausedState_TerminalLegal(t *testing.T) {
	ls := arkwenv1.LifecycleState(0).Descriptor()
	for i := 0; i < ls.Values().Len(); i++ {
		if contains(string(ls.Values().Get(i).Name()), "PAUSED") {
			t.Fatal("LifecycleState must not contain PAUSED (Invariant 8)")
		}
	}
	stream := goldenStream(t)
	term := stream[len(stream)-1].GetRunFinished().GetTermination()
	switch term.GetState() {
	case arkwenv1.TerminalState_TERMINAL_STATE_COMPLETED,
		arkwenv1.TerminalState_TERMINAL_STATE_FAILED,
		arkwenv1.TerminalState_TERMINAL_STATE_CANCELED:
	default:
		t.Fatalf("illegal terminal state %v", term.GetState())
	}
}

// G7: durable sanitized worker.raw is present (Invariant 3).
func TestG7_WorkerRawPresent(t *testing.T) {
	for _, e := range goldenStream(t) {
		if p, ok := e.GetPayload().(*arkwenv1.EventEnvelope_WorkerRaw); ok {
			if p.WorkerRaw.GetRawRef().GetContentHash() != nil {
				return
			}
		}
	}
	t.Fatal("no worker.raw event with a rawRef pointer")
}

// G8: provisioning re-verifies the frozen isolation contract (never re-resolves).
func TestG8_ProvisioningReverifiesContract(t *testing.T) {
	stream := goldenStream(t)
	var created, prov string
	for _, e := range stream {
		switch p := e.GetPayload().(type) {
		case *arkwenv1.EventEnvelope_RunCreated:
			created = p.RunCreated.GetSeed().GetIsolationContractRef().GetHex()
		case *arkwenv1.EventEnvelope_RunProvisioning:
			prov = p.RunProvisioning.GetIsolation().GetContractRef().GetHex()
		}
	}
	if created == "" || prov == "" || created != prov {
		t.Fatalf("contract ref mismatch: created=%q provisioning=%q", created, prov)
	}
}

// G9: consumer-agnostic + no callout fields (Invariants 1/10).
func TestG9_ConsumerAgnostic(t *testing.T) {
	if f := fieldsMatching("", "url", "callback", "webhook", "consumer"); len(f) != 0 {
		t.Fatalf("callout/consumer field(s) present (Invariant 10): %v", f)
	}
	// Principal exposes only type/principal_id/tenant_id.
	pd := (&arkwenv1.Principal{}).ProtoReflect().Descriptor()
	if pd.Fields().Len() != 3 {
		t.Fatalf("Principal must have exactly 3 fields, got %d", pd.Fields().Len())
	}
}

func contains(s, sub string) bool { return len(sub) > 0 && stringIndex(s, sub) >= 0 }

func stringIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
