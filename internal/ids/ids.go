// Package ids centralizes identity + hashing helpers used across Arkwen.
//
// Content addressing (ADR-004/007): the sha256 Digest IS identity. Run ids are
// derived deterministically from the enqueue idempotency key so that "same key
// => same run" (ADR-008 E6) holds structurally, with no mutable shadow state
// (Invariant 2): a duplicate enqueue produces the same run_id and its run.created
// at seq 0 collides with UNIQUE(run_id, seq) and is treated as idempotent.
package ids

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
)

// Sha256 returns the content-addressed Digest of data.
func Sha256(data []byte) *arkwenv1.Digest {
	sum := sha256.Sum256(data)
	return &arkwenv1.Digest{
		Algorithm: arkwenv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA256,
		Hex:       hex.EncodeToString(sum[:]),
	}
}

// Sha256Hex returns the lower-hex sha256 of data.
func Sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// RunIDFromKey derives a stable run id from an idempotency key. Empty key => a
// random run id (still unique). The derivation is total and deterministic so the
// idempotency guarantee needs no cache.
func RunIDFromKey(idempotencyKey string) string {
	if idempotencyKey == "" {
		return "run-" + randHex(12)
	}
	sum := sha256.Sum256([]byte("arkwen-run\x00" + idempotencyKey))
	return "run-" + hex.EncodeToString(sum[:])[:24]
}

// LeaseID / GateID / generic short ids for non-truth-bearing handles.
func Short(prefix string) string { return prefix + "-" + randHex(8) }

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is fatal for id generation; surface loudly.
		panic(fmt.Sprintf("ids: rand: %v", err))
	}
	return hex.EncodeToString(b)
}
