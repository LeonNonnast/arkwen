package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"github.com/arkwen/arkwen/internal/cas"
	"github.com/arkwen/arkwen/internal/ids"
)

// hex64 matches a resolved sha256 digest. A seed Digest must be a resolved
// content hash, NEVER a floating channel alias like "channel:tested"
// (reproduce-determinism D3): a channel is resolved to a digest exactly once at
// run.created and frozen (Invariant 6).
var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

const adapterVersion = "cell-shim/0.1.0"

// snapshotWorkbench materializes the workbench directory into a reproducible
// content-addressed snapshot (ADR-004 snapshot-first). It stores each file in the
// CAS and a sorted "path\thash" manifest, whose digest IS the snapshot ref. An
// empty/absent dir yields the digest of the empty manifest (still reproducible).
func snapshotWorkbench(ctx context.Context, store cas.Store, dir string) (*arkwenv1.Digest, error) {
	var lines []string
	if dir != "" {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					if d.Name() == ".git" {
						return filepath.SkipDir
					}
					return nil
				}
				data, err := os.ReadFile(p)
				if err != nil {
					return err
				}
				rel, _ := filepath.Rel(dir, p)
				ref, err := store.Put(ctx, rel, data, "application/octet-stream")
				if err != nil {
					return err
				}
				lines = append(lines, rel+"\t"+ref.GetContentHash().GetHex())
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("controller: snapshot workbench: %w", err)
			}
		}
	}
	sort.Strings(lines)
	manifest := ""
	for _, l := range lines {
		manifest += l + "\n"
	}
	ref, err := store.Put(ctx, "workbench.manifest", []byte(manifest), "text/plain")
	if err != nil {
		return nil, err
	}
	return ref.GetContentHash(), nil
}

// ValidateSeed is the exported guard used by conformance tests (reproduce-
// determinism D3): a seed field carrying a floating channel alias instead of a
// resolved digest is rejected.
func ValidateSeed(seed *arkwenv1.RunSeed) error { return validateSeed(seed) }

// validateSeed enforces that every content-addressed field is a resolved digest,
// never a floating alias (D3), and that mandatory fields are present.
func validateSeed(seed *arkwenv1.RunSeed) error {
	if seed.GetTenantId() == "" {
		return fmt.Errorf("controller: seed missing tenant_id (mandatory, ADR-010 E2)")
	}
	digests := map[string]*arkwenv1.Digest{
		"mission_hash":           seed.GetMissionHash(),
		"image_digest":           seed.GetImageDigest(),
		"policy_version":         seed.GetPolicyVersion(),
		"blueprint_digest":       seed.GetBlueprintDigest(),
		"isolation_contract_ref": seed.GetIsolationContractRef(),
		"authz_policy_version":   seed.GetAuthzPolicyVersion(),
		"egress_policy_hash":     seed.GetEgressPolicyHash(),
		"resource_limits_hash":   seed.GetResourceLimitsHash(),
		"secret_scope_set_ref":   seed.GetSecretScopeSetRef(),
	}
	if seed.GetWorkbenchSnapshotRef() != nil {
		digests["workbench_snapshot_ref"] = seed.GetWorkbenchSnapshotRef()
	}
	for name, d := range digests {
		if d == nil {
			return fmt.Errorf("controller: seed field %s not resolved", name)
		}
		if d.GetAlgorithm() != arkwenv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA256 {
			return fmt.Errorf("controller: seed field %s has non-sha256 algorithm", name)
		}
		if !hex64.MatchString(d.GetHex()) {
			return fmt.Errorf("controller: seed field %s is not a resolved digest (got %q — floating alias rejected)", name, d.GetHex())
		}
	}
	for _, tk := range seed.GetToolkitVersions() {
		if tk.GetDigest() == nil || !hex64.MatchString(tk.GetDigest().GetHex()) {
			return fmt.Errorf("controller: toolkit %q not resolved to a digest", tk.GetName())
		}
	}
	return nil
}

// imageDigestForWorker derives a reproducible worker-image digest from the worker
// kind (in production this is the real Worker Image digest from the Warehouse).
func imageDigestForWorker(kind string) *arkwenv1.Digest {
	return ids.Sha256([]byte("worker-image\x00" + kind))
}

// provenanceRef builds a content-addressed provenance pointer (Sigstore/SLSA) for
// the worker image — a ref only, never inline material (Invariant 4/5).
func provenanceRef(kind, worker, mime string) *arkwenv1.ContentRef {
	d := ids.Sha256([]byte("provenance\x00" + kind + "\x00" + worker))
	return &arkwenv1.ContentRef{
		Path:        "provenance/" + kind + ".json",
		ContentHash: d,
		MimeType:    mime,
		ArtifactRef: cas.Ref(d.GetHex()),
	}
}

// scopeSetRef is the audit-scope digest of the requested secret scopes (never the
// secrets themselves — Invariant 5).
func scopeSetRef(names []string) *arkwenv1.Digest {
	sort.Strings(names)
	s := ""
	for _, n := range names {
		s += n + "\n"
	}
	return ids.Sha256([]byte("secret-scope-set\x00" + s))
}
