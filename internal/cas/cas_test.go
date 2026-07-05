package cas

import (
	"context"
	"testing"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
)

// Invariant 4: content addressing — identical bytes yield identical digest/ref;
// Get round-trips and verifies on read.
func TestPutGetRoundtripAndIdentity(t *testing.T) {
	ctx := context.Background()
	for _, s := range []Store{NewMem()} {
		ref, err := s.Put(ctx, "a.txt", []byte("hello"), "text/plain")
		if err != nil {
			t.Fatal(err)
		}
		ref2, _ := s.Put(ctx, "b.txt", []byte("hello"), "text/plain")
		if ref.GetContentHash().GetHex() != ref2.GetContentHash().GetHex() {
			t.Fatal("identical bytes must have identical digest")
		}
		got, err := s.Get(ctx, ref)
		if err != nil || string(got) != "hello" {
			t.Fatalf("roundtrip failed: %q %v", got, err)
		}
	}
}

func TestGetDigestRejectsUnverifiable(t *testing.T) {
	ctx := context.Background()
	s := NewMem()
	_, err := s.GetDigest(ctx, &arkwenv1.Digest{Algorithm: arkwenv1.DigestAlgorithm_DIGEST_ALGORITHM_UNSPECIFIED, Hex: "00"})
	if err == nil {
		t.Fatal("unverifiable digest algorithm must be rejected (fail-closed)")
	}
}

func TestGetMissingIsNotFound(t *testing.T) {
	ctx := context.Background()
	s := NewMem()
	_, err := s.GetDigest(ctx, &arkwenv1.Digest{Algorithm: arkwenv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA256, Hex: "deadbeef"})
	if err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
