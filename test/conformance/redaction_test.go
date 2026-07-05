package conformance

import (
	"context"
	"strings"
	"testing"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"github.com/arkwen/arkwen/internal/app"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// collectRefs returns every ContentRef carried by the stream.
func collectRefs(evs []*arkwenv1.EventEnvelope) []*arkwenv1.ContentRef {
	var refs []*arkwenv1.ContentRef
	add := func(r *arkwenv1.ContentRef) {
		if r != nil {
			refs = append(refs, r)
		}
	}
	for _, e := range evs {
		switch p := e.GetPayload().(type) {
		case *arkwenv1.EventEnvelope_WorkerRaw:
			add(p.WorkerRaw.GetRawRef())
		case *arkwenv1.EventEnvelope_WorkerMessage:
			add(p.WorkerMessage.GetMessageRef())
		case *arkwenv1.EventEnvelope_WorkerToolCall:
			add(p.WorkerToolCall.GetArgumentsRef())
		case *arkwenv1.EventEnvelope_WorkerToolResult:
			add(p.WorkerToolResult.GetResultRef())
		case *arkwenv1.EventEnvelope_WorkerArtifactWritten:
			add(p.WorkerArtifactWritten.GetArtifact())
		}
	}
	return refs
}

// R1 + R2: a real run prints the injected secret; it lands on NO persistent
// surface, and worker.raw survives with the secret span replaced by a token.
func TestR1_R2_NoSecretOnAnySurface(t *testing.T) {
	rt := app.New()
	ctx := context.Background()
	missionRef, err := rt.CAS.Put(ctx, "mission.md", []byte("call the model"), "text/markdown")
	if err != nil {
		t.Fatal(err)
	}
	spec := &arkwenv1.RunSpec{MissionRef: missionRef, TenantId: "acme", MaterializationMode: arkwenv1.MaterializationMode_MATERIALIZATION_MODE_SNAPSHOT}
	runID, _, err := rt.Controller.Enqueue(ctx, spec, app.EnqueueOpts("claude-code", "", "acme"))
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Controller.Drive(ctx, runID); err != nil {
		t.Fatal(err)
	}
	evs, err := rt.Controller.Events(ctx, runID, 0)
	if err != nil {
		t.Fatal(err)
	}

	secret := app.DemoModelAPIKey

	// (a) the secret literal appears in ZERO serialized events (binary + JSON)
	for _, e := range evs {
		bin, _ := proto.Marshal(e)
		js, _ := protojson.Marshal(e)
		if strings.Contains(string(bin), secret) || strings.Contains(string(js), secret) {
			t.Fatalf("secret leaked into event seq %d", e.GetSeq())
		}
	}

	// (b) the secret literal appears in ZERO CAS blobs referenced by the stream
	sawRedactionToken := false
	sawWorkerRaw := false
	for _, ref := range collectRefs(evs) {
		data, err := rt.CAS.Get(ctx, ref)
		if err != nil {
			t.Fatalf("resolve ref %s: %v", ref.GetArtifactRef(), err)
		}
		if strings.Contains(string(data), secret) {
			t.Fatalf("secret leaked into CAS blob %s: %q", ref.GetPath(), string(data))
		}
		if strings.Contains(string(data), "<redacted:MODEL_API_KEY>") {
			sawRedactionToken = true
		}
	}

	// (c) worker.raw is present and preserves non-secret bytes with the secret span redacted
	for _, e := range evs {
		if p, ok := e.GetPayload().(*arkwenv1.EventEnvelope_WorkerRaw); ok {
			sawWorkerRaw = true
			data, err := rt.CAS.Get(ctx, p.WorkerRaw.GetRawRef())
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "Calling model API with key") &&
				!strings.Contains(string(data), "<redacted:MODEL_API_KEY>") {
				t.Fatalf("worker.raw kept the secret span unredacted: %q", string(data))
			}
		}
	}
	if !sawWorkerRaw {
		t.Fatal("no worker.raw (Invariant 3: durable sanitized raw must survive)")
	}
	if !sawRedactionToken {
		t.Fatal("expected a <redacted:MODEL_API_KEY> token in some raw blob (fidelity preserved, secret excised)")
	}

	// (d) projections carry no secret
	for _, k := range []arkwenv1.ProjectionKind{
		arkwenv1.ProjectionKind_PROJECTION_KIND_RUN_SUMMARY,
		arkwenv1.ProjectionKind_PROJECTION_KIND_ARTIFACT_MANIFEST,
		arkwenv1.ProjectionKind_PROJECTION_KIND_STATUS,
	} {
		pr, err := rt.Controller.Projection(ctx, runID, k)
		if err != nil {
			t.Fatal(err)
		}
		js, _ := protojson.Marshal(pr)
		if strings.Contains(string(js), secret) {
			t.Fatalf("secret leaked into projection %v", k)
		}
	}
}

// R3: the security payloads structurally cannot hold a secret value — they carry
// only channel/counts/rule_id and (for leases) a scope Digest + expiry.
func TestR3_SecurityPayloadsMetadataOnly(t *testing.T) {
	expect := map[string][]string{
		"SecretLeakDetected": {"channel", "match_count"},
		"RedactionApplied":   {"channel", "redaction_count", "rule_id"},
		"SecretLeased":       {"lease_id", "secret_scope_ref", "expires_at"},
	}
	for msg, want := range expect {
		got := fieldSet(t, msg)
		if !sameSet(got, want) {
			t.Fatalf("%s fields = %v, want exactly %v (metadata only, Invariant 5)", msg, got, want)
		}
	}
}

// R4: contract-plane structural secret exclusion — no credential field exists on
// Principal or the audit ledger (a distinct mechanism from Cell-Shim redaction).
func TestR4_AuditLedgerStructuralExclusion(t *testing.T) {
	if got := fieldSet(t, "Principal"); !sameSet(got, []string{"type", "principal_id", "tenant_id"}) {
		t.Fatalf("Principal fields = %v (must be non-secret id only)", got)
	}
	for _, msg := range []string{"Principal", "ControlPlaneAuditLedgerEnvelope", "AuthDecision", "FloorChanged"} {
		if f := fieldsMatching(msg, "token", "credential", "password", "bearer", "apikey"); len(f) != 0 {
			t.Fatalf("%s has a credential-bearing field %v (structural exclusion violated)", msg, f)
		}
	}
}

// R5: no bytes content field anywhere ⇒ persist-before-redact is impossible.
func TestR5_NoBytesContentField(t *testing.T) {
	if bf := bytesContentFields(); len(bf) != 0 {
		t.Fatalf("bytes content field(s) present: %v", bf)
	}
}

// fieldSet returns the proto field names of an arkwen.v1 message by simple name.
func fieldSet(t *testing.T, messageName string) []string {
	t.Helper()
	var out []string
	rangeArkwenMessages(func(md protoreflect.MessageDescriptor) {
		if string(md.Name()) != messageName {
			return
		}
		fields := md.Fields()
		for i := 0; i < fields.Len(); i++ {
			out = append(out, string(fields.Get(i).Name()))
		}
	})
	if out == nil {
		t.Fatalf("message %s not found in descriptor set", messageName)
	}
	return out
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	for _, v := range m {
		if v != 0 {
			return false
		}
	}
	return true
}
