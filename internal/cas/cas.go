// Package cas is the content-addressed store — the shared substrate behind the
// Warehouse and the Artifact Store (ADR-004/007). The sha256 Digest IS identity:
// equal hex => equal bytes. This is where all content lives; the event stream and
// wire contracts carry only ContentRef/Digest pointers (Invariant 4). Nothing
// here ever stores an event; it stores blobs keyed by their own hash.
package cas

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"github.com/arkwen/arkwen/internal/ids"
)

// ErrNotFound is returned when a ref resolves to no content.
var ErrNotFound = errors.New("cas: content not found")

// Store is the content-addressed blob store. Put is idempotent by content:
// putting the same bytes twice yields the same ref.
type Store interface {
	// Put stores data and returns a ContentRef whose content_hash is sha256(data).
	// path/mime are metadata carried on the ref (Invariant 4: the wire never holds
	// the bytes, only this pointer).
	Put(ctx context.Context, path string, data []byte, mime string) (*arkwenv1.ContentRef, error)
	// Get resolves a ref to its bytes, verifying the digest on read.
	Get(ctx context.Context, ref *arkwenv1.ContentRef) ([]byte, error)
	// GetDigest resolves a bare digest to its bytes.
	GetDigest(ctx context.Context, d *arkwenv1.Digest) ([]byte, error)
	// Has reports whether the digest is present.
	Has(ctx context.Context, d *arkwenv1.Digest) bool
}

// Ref builds the canonical CAS locator "cas://sha256/<hex>".
func Ref(hexDigest string) string { return "cas://sha256/" + hexDigest }

func refFor(path string, data []byte, mime string) *arkwenv1.ContentRef {
	d := ids.Sha256(data)
	return &arkwenv1.ContentRef{
		Path:        path,
		ContentHash: d,
		SizeBytes:   uint64(len(data)),
		MimeType:    mime,
		ArtifactRef: Ref(d.GetHex()),
	}
}

// ---- in-memory store (default for the walking skeleton + tests) ----

type memStore struct {
	mu   sync.RWMutex
	blob map[string][]byte // hex -> bytes
}

// NewMem returns an in-memory content-addressed store.
func NewMem() Store { return &memStore{blob: map[string][]byte{}} }

func (m *memStore) Put(_ context.Context, path string, data []byte, mime string) (*arkwenv1.ContentRef, error) {
	ref := refFor(path, data, mime)
	m.mu.Lock()
	defer m.mu.Unlock()
	// copy to defeat aliasing; content-addressed => a present key is byte-identical.
	if _, ok := m.blob[ref.GetContentHash().GetHex()]; !ok {
		cp := make([]byte, len(data))
		copy(cp, data)
		m.blob[ref.GetContentHash().GetHex()] = cp
	}
	return ref, nil
}

func (m *memStore) Get(ctx context.Context, ref *arkwenv1.ContentRef) ([]byte, error) {
	return m.GetDigest(ctx, ref.GetContentHash())
}

func (m *memStore) GetDigest(_ context.Context, d *arkwenv1.Digest) ([]byte, error) {
	if d.GetAlgorithm() != arkwenv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA256 {
		return nil, fmt.Errorf("cas: unverifiable digest algorithm %v", d.GetAlgorithm())
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.blob[d.GetHex()]
	if !ok {
		return nil, ErrNotFound
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out, nil
}

func (m *memStore) Has(_ context.Context, d *arkwenv1.Digest) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.blob[d.GetHex()]
	return ok
}

// ---- filesystem store (durable Artifact Store / Warehouse substrate) ----

type fsStore struct {
	root string
}

// NewFS returns a filesystem-backed content-addressed store rooted at dir.
func NewFS(dir string) (Store, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("cas: mkdir: %w", err)
	}
	return &fsStore{root: dir}, nil
}

func (f *fsStore) path(hexDigest string) string {
	// shard by first 2 hex chars to avoid huge directories
	return filepath.Join(f.root, hexDigest[:2], hexDigest)
}

func (f *fsStore) Put(_ context.Context, path string, data []byte, mime string) (*arkwenv1.ContentRef, error) {
	ref := refFor(path, data, mime)
	p := f.path(ref.GetContentHash().GetHex())
	if _, err := os.Stat(p); err == nil {
		return ref, nil // already present; content-addressed => identical
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return nil, fmt.Errorf("cas: mkdir: %w", err)
	}
	if err := os.WriteFile(p, data, 0o640); err != nil {
		return nil, fmt.Errorf("cas: write: %w", err)
	}
	return ref, nil
}

func (f *fsStore) Get(ctx context.Context, ref *arkwenv1.ContentRef) ([]byte, error) {
	return f.GetDigest(ctx, ref.GetContentHash())
}

func (f *fsStore) GetDigest(_ context.Context, d *arkwenv1.Digest) ([]byte, error) {
	if d.GetAlgorithm() != arkwenv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA256 {
		return nil, fmt.Errorf("cas: unverifiable digest algorithm %v", d.GetAlgorithm())
	}
	b, err := os.ReadFile(f.path(d.GetHex()))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("cas: read: %w", err)
	}
	// verify on read (content addressing is a guarantee, not a hint)
	if got := ids.Sha256Hex(b); got != d.GetHex() {
		return nil, fmt.Errorf("cas: digest mismatch: want %s got %s", d.GetHex(), got)
	}
	return b, nil
}

func (f *fsStore) Has(_ context.Context, d *arkwenv1.Digest) bool {
	_, err := os.Stat(f.path(d.GetHex()))
	return err == nil
}
