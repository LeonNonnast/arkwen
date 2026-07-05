package warehouse

import (
	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
)

// DigestsFromSeed returns every content-addressed reference a frozen seed holds
// (ADR-009 R7): these are GC roots — the transitive closure of all persisted seed
// refs keeps their blobs immortal (Invariants 3/4/6).
func DigestsFromSeed(seed *arkwenv1.RunSeed) []*arkwenv1.Digest {
	if seed == nil {
		return nil
	}
	var out []*arkwenv1.Digest
	add := func(d *arkwenv1.Digest) {
		if d != nil && d.GetHex() != "" {
			out = append(out, d)
		}
	}
	add(seed.GetMissionHash())
	add(seed.GetImageDigest())
	add(seed.GetWorkbenchSnapshotRef())
	add(seed.GetPolicyVersion())
	add(seed.GetBlueprintDigest())
	add(seed.GetIsolationContractRef())
	add(seed.GetAuthzPolicyVersion())
	add(seed.GetEgressPolicyHash())
	add(seed.GetResourceLimitsHash())
	add(seed.GetSecretScopeSetRef())
	for _, t := range seed.GetToolkitVersions() {
		add(t.GetDigest())
	}
	if r := seed.GetImageSignatureRef(); r != nil {
		add(r.GetContentHash())
	}
	if r := seed.GetImageProvenanceRef(); r != nil {
		add(r.GetContentHash())
	}
	return out
}

// DigestsFromEvents returns every content pointer carried by an event stream
// (artifact_ref / worker.raw / message / tool refs). Event-stream pointers are GC
// roots too — worker.raw and artifacts are never collected while the run exists
// (Invariants 3/4).
func DigestsFromEvents(events []*arkwenv1.EventEnvelope) []*arkwenv1.Digest {
	var out []*arkwenv1.Digest
	add := func(r *arkwenv1.ContentRef) {
		if r != nil && r.GetContentHash() != nil {
			out = append(out, r.GetContentHash())
		}
	}
	for _, e := range events {
		switch p := e.GetPayload().(type) {
		case *arkwenv1.EventEnvelope_RunCreated:
			out = append(out, DigestsFromSeed(p.RunCreated.GetSeed())...)
		case *arkwenv1.EventEnvelope_WorkerMessage:
			add(p.WorkerMessage.GetMessageRef())
		case *arkwenv1.EventEnvelope_WorkerToolCall:
			add(p.WorkerToolCall.GetArgumentsRef())
		case *arkwenv1.EventEnvelope_WorkerToolResult:
			add(p.WorkerToolResult.GetResultRef())
		case *arkwenv1.EventEnvelope_WorkerArtifactWritten:
			add(p.WorkerArtifactWritten.GetArtifact())
		case *arkwenv1.EventEnvelope_WorkerRaw:
			add(p.WorkerRaw.GetRawRef())
		}
	}
	return out
}

// GC returns the digests the warehouse can collect: every stored blob that is NOT
// in the transitive reachable set from the given roots (ADR-009 R7). A blob
// referenced by ANY root (a seed ref, a channel pointer, an event pointer)
// survives; only when the LAST reference is dropped does it become collectable.
// This is the generic reference-based rule — not an enumerated subset.
func (w *Warehouse) GC(roots ...*arkwenv1.Digest) []*arkwenv1.Digest {
	reachable := map[string]bool{}
	for _, d := range roots {
		if d != nil {
			reachable[d.GetHex()] = true
		}
	}
	// channel pointers are always roots (a channel keeps its target alive)
	for _, d := range w.channelPointers() {
		reachable[d.GetHex()] = true
	}
	var collectable []*arkwenv1.Digest
	for _, d := range w.storedDigests() {
		if !reachable[d.GetHex()] {
			collectable = append(collectable, d)
		}
	}
	return collectable
}
