package warehouse

import (
	"fmt"
	"regexp"
	"sort"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
)

// Classification annotates a reproduce() RESULT (ADR-007 E6). It is deliberately
// NOT a lifecycle or terminal state (Invariant 8): DEGRADED lives outside the
// wire enums and never becomes a run state.
type Classification string

const (
	// Reproduced: every input resolved to a digest -> byte-identical inputs.
	Reproduced Classification = "REPRODUCED"
	// Degraded: an input is not bit-reproducible (mount mode) — a classification,
	// never a state.
	Degraded Classification = "DEGRADED"
)

var resolvedHex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ErrUnresolvedSeed marks a seed whose refs are not resolved digests (a floating
// channel alias in a seed is rejected — reproduce-determinism D3, Invariant 6).
var ErrUnresolvedSeed = fmt.Errorf("warehouse: seed carries an unresolved reference (floating alias)")

// Reproduce resolves every RunSeed field to bytes-identity inputs (ADR-007 E6).
// It is a PURE function of the frozen seed: the same seed yields byte-identical
// resolvedInputs on every call (D1), the strict result reuses the frozen
// session_seed rather than drawing a fresh one (D2), and a mount-mode seed is
// classified DEGRADED (D4/D5). A floating-alias snapshot ref is rejected (D3).
func Reproduce(seed *arkwenv1.RunSeed) (Classification, map[string]string, error) {
	if seed == nil {
		return Degraded, nil, ErrUnresolvedSeed
	}
	// A mount baseline is explicitly NOT bit-reproducible (ADR-007 E6).
	if seed.GetMaterializationMode() == arkwenv1.MaterializationMode_MATERIALIZATION_MODE_MOUNT ||
		seed.GetMaterializationMode() == arkwenv1.MaterializationMode_MATERIALIZATION_MODE_LIVE_BIND ||
		seed.GetWorkbenchMountRef() != "" {
		return Degraded, map[string]string{
			"reason": "materializationMode MOUNT (workbenchMountRef) is not bit-reproducible (ADR-007 E6)",
		}, nil
	}
	// Every content-addressed input must be a resolved digest (never a channel).
	snap := seed.GetWorkbenchSnapshotRef()
	if snap == nil || !resolvedHex.MatchString(snap.GetHex()) {
		return Degraded, nil, fmt.Errorf("%w: workbench_snapshot_ref=%q", ErrUnresolvedSeed, snap.GetHex())
	}
	for _, d := range []*arkwenv1.Digest{seed.GetMissionHash(), seed.GetImageDigest()} {
		if d == nil || !resolvedHex.MatchString(d.GetHex()) {
			return Degraded, nil, ErrUnresolvedSeed
		}
	}
	inputs := map[string]string{
		"mission":     seed.GetMissionHash().GetHex(),
		"image":       seed.GetImageDigest().GetHex(),
		"workbench":   snap.GetHex(),
		"sessionSeed": seed.GetSessionSeed(), // strict reuses the FROZEN session seed (D2)
	}
	// toolkits are resolved deterministically by name
	names := make([]string, 0, len(seed.GetToolkitVersions()))
	tk := map[string]string{}
	for _, t := range seed.GetToolkitVersions() {
		if t.GetDigest() == nil || !resolvedHex.MatchString(t.GetDigest().GetHex()) {
			return Degraded, nil, ErrUnresolvedSeed
		}
		names = append(names, t.GetName())
		tk[t.GetName()] = t.GetDigest().GetHex()
	}
	sort.Strings(names)
	for _, n := range names {
		inputs["toolkit."+n] = tk[n]
	}
	return Reproduced, inputs, nil
}
