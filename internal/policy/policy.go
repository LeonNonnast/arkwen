// Package policy composes the per-layer RunSpec policy bundle into the FROZEN
// isolation contract + materialized gate set + their content-addressed digests
// (ADR-005/006 + ADR-009 R2/R3). It is where Invariant 7/10 get teeth:
//
//   - Composition is stricter-only across ORG > BLUEPRINT > MISSION:
//     profile = max(), egress = intersection, resource ceiling = min(),
//     image_trust = OR.
//   - A lower layer that tries to LOOSEN a higher layer (weaker profile, ALLOW
//     over DENY, a fail_open gate over fail_closed, a raised resource ceiling)
//     is REJECTED at enqueue — never silently corrected (ErrLoosensFloor).
//   - The intrinsic Arkwen floor sits BELOW org and is not expressible as a
//     field: profile >= STANDARD, egress default-deny, redaction always on.
package policy

import (
	"errors"
	"fmt"
	"sort"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"github.com/arkwen/arkwen/internal/ids"
	"google.golang.org/protobuf/proto"
)

// ErrLoosensFloor is returned when a lower policy layer would loosen a higher
// one. The enqueue is rejected; the caller records the denial (never a run event).
var ErrLoosensFloor = errors.New("policy: lower layer loosens a higher layer floor (rejected, not corrected)")

// The S0 intrinsic minimal egress allowlist — the model-API endpoint (the live
// credential's sole destination). Kept default-deny for everything else.
const (
	modelAPIHost = "api.anthropic.com"
	modelAPIPort = 443
)

// Composed is the frozen output of composition, ready to seed run.created.
type Composed struct {
	Isolation            *arkwenv1.IsolationContract
	Gates                []*arkwenv1.GateRule
	IsolationContractRef *arkwenv1.Digest
	PolicyVersion        *arkwenv1.Digest // gate-set digest ONLY (ADR-009 R2)
	EgressPolicyHash     *arkwenv1.Digest
	ResourceLimitsHash   *arkwenv1.Digest
}

// digest returns the deterministic content-addressed digest of a proto message.
func digest(m proto.Message) (*arkwenv1.Digest, error) {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if err != nil {
		return nil, err
	}
	return ids.Sha256(b), nil
}

// Compose composes the bundle stricter-only and freezes the digests. A nil
// bundle is treated as an all-defaults bundle (intrinsic floor only).
func Compose(bundle *arkwenv1.PolicyBundle) (*Composed, error) {
	layers := orderedLayers(bundle)

	iso, err := composeIsolation(layers)
	if err != nil {
		return nil, err
	}
	gates, err := composeGates(layers)
	if err != nil {
		return nil, err
	}

	// content-addressed digests (frozen into RunSeed)
	egressHash, err := digest(iso.GetEgress())
	if err != nil {
		return nil, err
	}
	resHash, err := digest(iso.GetResourceLimits())
	if err != nil {
		return nil, err
	}
	// isolation_contract_ref = hash of the contract with contract_ref cleared.
	isoCopy := proto.Clone(iso).(*arkwenv1.IsolationContract)
	isoCopy.ContractRef = nil
	isoRef, err := digest(isoCopy)
	if err != nil {
		return nil, err
	}
	iso.ContractRef = isoRef

	// policy_version = digest of the sorted materialized gate set.
	gateSet := &arkwenv1.PolicyLayer{Gates: gates}
	polVer, err := digest(gateSet)
	if err != nil {
		return nil, err
	}

	return &Composed{
		Isolation:            iso,
		Gates:                gates,
		IsolationContractRef: isoRef,
		PolicyVersion:        polVer,
		EgressPolicyHash:     egressHash,
		ResourceLimitsHash:   resHash,
	}, nil
}

// orderedLayers returns the present layers in precedence order Org, Blueprint,
// Mission (highest authority first). Nil layers are skipped.
func orderedLayers(b *arkwenv1.PolicyBundle) []*arkwenv1.PolicyLayer {
	if b == nil {
		return nil
	}
	var out []*arkwenv1.PolicyLayer
	for _, l := range []*arkwenv1.PolicyLayer{b.GetOrg(), b.GetBlueprint(), b.GetMission()} {
		if l != nil {
			out = append(out, l)
		}
	}
	return out
}

// composeIsolation applies profile=max / egress=∩ / resource=min / image_trust=OR
// with a stricter-only guard. The intrinsic floor pins profile >= STANDARD and
// egress default-deny.
func composeIsolation(layers []*arkwenv1.PolicyLayer) (*arkwenv1.IsolationContract, error) {
	// intrinsic floor
	profile := arkwenv1.IsolationProfile_ISOLATION_PROFILE_STANDARD
	egress := &arkwenv1.EgressPolicy{DefaultAction: arkwenv1.EgressAction_EGRESS_ACTION_DENY}
	var res *arkwenv1.ResourceLimits
	trust := &arkwenv1.ImageTrust{}

	var ceilingAllow []*arkwenv1.EgressRule // running intersection ceiling
	first := true

	for _, l := range layers {
		in := l.GetIsolation()
		if in == nil {
			continue
		}
		// profile: stricter-only, compose via max
		if p := in.GetProfileFloor(); p != arkwenv1.IsolationProfile_ISOLATION_PROFILE_UNSPECIFIED {
			if p < profile {
				return nil, fmt.Errorf("%w: %v profile_floor %v < current floor %v", ErrLoosensFloor, l.GetLayer(), p, profile)
			}
			profile = p
		}
		// egress default_action: DENY is the floor; a layer flipping to ALLOW loosens.
		if e := in.GetEgress(); e != nil {
			if e.GetDefaultAction() == arkwenv1.EgressAction_EGRESS_ACTION_ALLOW && egress.GetDefaultAction() == arkwenv1.EgressAction_EGRESS_ACTION_DENY {
				return nil, fmt.Errorf("%w: %v egress default ALLOW over DENY", ErrLoosensFloor, l.GetLayer())
			}
			// allow-list: intersection (needs ∩ ceiling). An empty list = no opinion.
			if len(e.GetAllow()) > 0 {
				if first {
					ceilingAllow = cloneRules(e.GetAllow())
					first = false
				} else {
					ceilingAllow = intersectRules(ceilingAllow, e.GetAllow())
				}
			}
			egress.Deny = append(egress.GetDeny(), e.GetDeny()...)
		}
		// resource ceiling: min; a layer RAISING a ceiling over a higher layer loosens.
		r := in.GetResourceCeiling()
		if r != nil {
			var err error
			res, err = composeResource(res, r, l.GetLayer())
			if err != nil {
				return nil, err
			}
		}
		// image trust: OR (any layer requiring a signature wins)
		if in.GetImageTrust().GetRequireSignature() {
			trust.RequireSignature = true
			if s := in.GetImageTrust().GetSignatureRef(); s != nil {
				trust.SignatureRef = s
			}
			if pv := in.GetImageTrust().GetProvenanceRef(); pv != nil {
				trust.ProvenanceRef = pv
			}
		}
	}
	// Intrinsic minimal allowlist (S0 fail-closed floor, ADR-006): the worker's
	// sole credential destination — the model-API endpoint — is always reachable;
	// everything else is default-deny. An explicit deny still wins over it. In S2a
	// the allowlist is fully policy-composed above this floor.
	egress.Allow = ceilingAllow
	egress.Allow = unionRule(egress.Allow, &arkwenv1.EgressRule{Host: modelAPIHost, Port: modelAPIPort})
	egress.Allow = removeDenied(egress.Allow, egress.Deny)
	sortRules(egress.Allow)
	sort.Strings(egress.Deny)

	if res == nil {
		res = &arkwenv1.ResourceLimits{} // deterministic empty ceiling (never nil)
	}
	return &arkwenv1.IsolationContract{
		Profile:        profile,
		Egress:         egress,
		ResourceLimits: res,
		ImageTrust:     trust,
	}, nil
}

// composeResource takes the min of each dimension; a higher ceiling in a lower
// layer is a loosening (rejected). A zero means "unset / no ceiling from this
// layer" and does not lower the composed value.
func composeResource(cur, next *arkwenv1.ResourceLimits, layer arkwenv1.PolicyLayerKind) (*arkwenv1.ResourceLimits, error) {
	if cur == nil {
		return proto.Clone(next).(*arkwenv1.ResourceLimits), nil
	}
	out := proto.Clone(cur).(*arkwenv1.ResourceLimits)
	minU := func(name string, c, n uint64, set func(uint64)) error {
		if n == 0 {
			return nil
		}
		if c != 0 && n > c {
			return fmt.Errorf("%w: %v raises %s ceiling %d > %d", ErrLoosensFloor, layer, name, n, c)
		}
		set(n)
		return nil
	}
	if err := minU("cpu", cur.GetCpuMillicores(), next.GetCpuMillicores(), func(v uint64) { out.CpuMillicores = v }); err != nil {
		return nil, err
	}
	if err := minU("mem", cur.GetMemBytes(), next.GetMemBytes(), func(v uint64) { out.MemBytes = v }); err != nil {
		return nil, err
	}
	if err := minU("disk", cur.GetDiskBytes(), next.GetDiskBytes(), func(v uint64) { out.DiskBytes = v }); err != nil {
		return nil, err
	}
	if err := minU("pids", cur.GetPids(), next.GetPids(), func(v uint64) { out.Pids = v }); err != nil {
		return nil, err
	}
	return out, nil
}

// composeGates deduplicates gate rules by gate_id keeping the STRICTEST, and
// rejects a lower layer that loosens a higher-layer gate (fail_open over
// fail_closed, or mandatory:false over mandatory:true).
func composeGates(layers []*arkwenv1.PolicyLayer) ([]*arkwenv1.GateRule, error) {
	byID := map[string]*arkwenv1.GateRule{}
	var order []string
	for _, l := range layers {
		for _, g := range l.GetGates() {
			existing, ok := byID[g.GetGateId()]
			if !ok {
				byID[g.GetGateId()] = proto.Clone(g).(*arkwenv1.GateRule)
				order = append(order, g.GetGateId())
				continue
			}
			// stricter-only checks against the higher-precedence existing rule
			if existing.GetTimeoutPolicy() == arkwenv1.TimeoutPolicy_TIMEOUT_POLICY_FAIL_CLOSED &&
				g.GetTimeoutPolicy() == arkwenv1.TimeoutPolicy_TIMEOUT_POLICY_FAIL_OPEN {
				return nil, fmt.Errorf("%w: gate %s flips fail_closed -> fail_open", ErrLoosensFloor, g.GetGateId())
			}
			if existing.GetMandatory() && !g.GetMandatory() {
				return nil, fmt.Errorf("%w: gate %s drops mandatory", ErrLoosensFloor, g.GetGateId())
			}
			// otherwise keep the higher-precedence rule (already strictest)
		}
	}
	sort.Strings(order)
	out := make([]*arkwenv1.GateRule, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out, nil
}

func cloneRules(in []*arkwenv1.EgressRule) []*arkwenv1.EgressRule {
	out := make([]*arkwenv1.EgressRule, len(in))
	for i, r := range in {
		out[i] = proto.Clone(r).(*arkwenv1.EgressRule)
	}
	return out
}

func intersectRules(a, b []*arkwenv1.EgressRule) []*arkwenv1.EgressRule {
	set := map[string]bool{}
	for _, r := range b {
		set[ruleKey(r)] = true
	}
	var out []*arkwenv1.EgressRule
	for _, r := range a {
		if set[ruleKey(r)] {
			out = append(out, r)
		}
	}
	return out
}

func ruleKey(r *arkwenv1.EgressRule) string { return fmt.Sprintf("%s:%d", r.GetHost(), r.GetPort()) }

// unionRule adds r to the set unless already present (idempotent).
func unionRule(set []*arkwenv1.EgressRule, r *arkwenv1.EgressRule) []*arkwenv1.EgressRule {
	for _, x := range set {
		if ruleKey(x) == ruleKey(r) {
			return set
		}
	}
	return append(set, r)
}

// removeDenied drops any allow rule whose host is explicitly denied (deny wins).
func removeDenied(allow []*arkwenv1.EgressRule, deny []string) []*arkwenv1.EgressRule {
	if len(deny) == 0 {
		return allow
	}
	denied := map[string]bool{}
	for _, h := range deny {
		denied[h] = true
	}
	var out []*arkwenv1.EgressRule
	for _, r := range allow {
		if !denied[r.GetHost()] {
			out = append(out, r)
		}
	}
	return out
}

func sortRules(rs []*arkwenv1.EgressRule) {
	sort.Slice(rs, func(i, j int) bool { return ruleKey(rs[i]) < ruleKey(rs[j]) })
}
