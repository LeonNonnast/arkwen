package warehouse

import (
	"fmt"
	"sort"
	"strings"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"github.com/arkwen/arkwen/internal/ids"
)

// Blueprint is the reproducible run-template & sole pin-point (ADR-007 E3). Its
// governance/security-floor fields (gate_policy_set_ref, isolation inputs,
// required_capabilities) are stricter-only; its functional fields
// (worker_image_digest, toolkits, materialization_mode) are mission-overridable
// only within the mission_interface and only toward reproducible — each override
// is re-pinned + re-seeded. blueprint_digest == hash(manifest) (self-describing).
type Blueprint struct {
	Name                 string
	WorkerImageDigest    *arkwenv1.Digest
	Toolkits             []*arkwenv1.ToolkitPin
	MaterializationMode  arkwenv1.MaterializationMode
	GatePolicySetRef     *arkwenv1.Digest          // governance floor (stricter-only)
	Isolation            *arkwenv1.IsolationInputs // isolation-policy INPUTS (stricter-only)
	RequiredCapabilities []arkwenv1.Capability
}

// Manifest returns the canonical, deterministic self-describing manifest bytes.
// The encoding is stable (sorted) so blueprint_digest is reproducible.
func (b *Blueprint) Manifest() []byte {
	var sb strings.Builder
	fmt.Fprintf(&sb, "name=%s\n", b.Name)
	fmt.Fprintf(&sb, "worker_image=%s\n", hexOf(b.WorkerImageDigest))
	fmt.Fprintf(&sb, "materialization=%s\n", b.MaterializationMode.String())
	fmt.Fprintf(&sb, "gate_policy_set=%s\n", hexOf(b.GatePolicySetRef))
	tks := make([]string, 0, len(b.Toolkits))
	for _, t := range b.Toolkits {
		tks = append(tks, t.GetName()+"@"+hexOf(t.GetDigest()))
	}
	sort.Strings(tks)
	fmt.Fprintf(&sb, "toolkits=%s\n", strings.Join(tks, ","))
	caps := make([]string, 0, len(b.RequiredCapabilities))
	for _, c := range b.RequiredCapabilities {
		caps = append(caps, c.String())
	}
	sort.Strings(caps)
	fmt.Fprintf(&sb, "required_capabilities=%s\n", strings.Join(caps, ","))
	if b.Isolation != nil {
		fmt.Fprintf(&sb, "isolation_profile_floor=%s\n", b.Isolation.GetProfileFloor().String())
	}
	return []byte(sb.String())
}

// Digest returns blueprint_digest = sha256(manifest) — the single pin-point.
func (b *Blueprint) Digest() *arkwenv1.Digest { return ids.Sha256(b.Manifest()) }

func hexOf(d *arkwenv1.Digest) string {
	if d == nil {
		return ""
	}
	return d.GetHex()
}
