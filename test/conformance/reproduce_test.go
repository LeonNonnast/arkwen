package conformance

import (
	"testing"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"github.com/arkwen/arkwen/internal/controller"
)

// D3: a seed field carrying a floating channel alias instead of a resolved digest
// is rejected (channels/ranges are resolved exactly once at run.created and
// frozen — Invariant 6).
func TestD3_FloatingAliasRejected(t *testing.T) {
	good := &arkwenv1.RunSeed{
		TenantId:             "acme",
		MissionHash:          sha("mission"),
		ImageDigest:          sha("image"),
		PolicyVersion:        sha("policy"),
		BlueprintDigest:      sha("blueprint"),
		IsolationContractRef: sha("iso"),
		AuthzPolicyVersion:   sha("authz"),
		EgressPolicyHash:     sha("egress"),
		ResourceLimitsHash:   sha("res"),
		SecretScopeSetRef:    sha("scope"),
		WorkbenchBaseline:    &arkwenv1.RunSeed_WorkbenchSnapshotRef{WorkbenchSnapshotRef: sha("wb")},
	}
	if err := controller.ValidateSeed(good); err != nil {
		t.Fatalf("a fully-resolved seed must validate: %v", err)
	}
	// now poison the snapshot ref with a floating channel alias
	bad := cloneSeed(good)
	bad.WorkbenchBaseline = &arkwenv1.RunSeed_WorkbenchSnapshotRef{
		WorkbenchSnapshotRef: &arkwenv1.Digest{Algorithm: arkwenv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA256, Hex: "channel:tested"},
	}
	if err := controller.ValidateSeed(bad); err == nil {
		t.Fatal("a seed carrying a floating channel alias must be rejected (D3)")
	}
}

// D4: DEGRADED is a result classification, never a lifecycle/terminal STATE.
func TestD4_DegradedNotAState(t *testing.T) {
	ls := arkwenv1.LifecycleState(0).Descriptor()
	for i := 0; i < ls.Values().Len(); i++ {
		if contains(string(ls.Values().Get(i).Name()), "DEGRADED") {
			t.Fatal("DEGRADED must not be a LifecycleState")
		}
	}
	ts := arkwenv1.TerminalState(0).Descriptor()
	for i := 0; i < ts.Values().Len(); i++ {
		if contains(string(ts.Values().Get(i).Name()), "DEGRADED") {
			t.Fatal("DEGRADED must not be a TerminalState")
		}
	}
}

func sha(s string) *arkwenv1.Digest {
	// 64-hex resolved digest derived from s (deterministic).
	h := "0000000000000000000000000000000000000000000000000000000000000000"
	x := []byte(h)
	for i, c := range []byte(s) {
		x[i%64] = "0123456789abcdef"[int(c)%16]
	}
	return &arkwenv1.Digest{Algorithm: arkwenv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA256, Hex: string(x)}
}

func cloneSeed(s *arkwenv1.RunSeed) *arkwenv1.RunSeed {
	return &arkwenv1.RunSeed{
		TenantId:             s.GetTenantId(),
		MissionHash:          s.GetMissionHash(),
		ImageDigest:          s.GetImageDigest(),
		PolicyVersion:        s.GetPolicyVersion(),
		BlueprintDigest:      s.GetBlueprintDigest(),
		IsolationContractRef: s.GetIsolationContractRef(),
		AuthzPolicyVersion:   s.GetAuthzPolicyVersion(),
		EgressPolicyHash:     s.GetEgressPolicyHash(),
		ResourceLimitsHash:   s.GetResourceLimitsHash(),
		SecretScopeSetRef:    s.GetSecretScopeSetRef(),
	}
}
