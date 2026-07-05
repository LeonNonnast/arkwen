// Package warehouse is the content-addressed Warehouse (ADR-007): ONE CAS
// substrate with TWO catalogs — Warehouse-Inputs (curated/signed worker images,
// toolkits, blueprints, the Cell-Shim binary) and the Artifact Store (run outputs
// + workbench snapshots). They share the blob substrate but keep separate
// namespaces + lifecycles.
//
// Versioning (ADR-007 E2 / ADR-009 R1): the sha256 Digest is the SOLE identity.
// Exact aliases are immutable-by-policy; channels (dev/tested/released) are
// MOVABLE pointers. An alias/channel is resolved to a Digest EXACTLY ONCE at
// run.created and frozen into the seed; replay never uses a floating pointer.
package warehouse

import (
	"context"
	"errors"
	"fmt"
	"sync"

	arkwenv1 "github.com/arkwen/arkwen/gen/go/arkwen/v1"
	"github.com/arkwen/arkwen/internal/cas"
)

// Catalog names the two disjoint namespaces over the shared CAS substrate.
type Catalog int

const (
	CatalogInputs    Catalog = iota // curated inputs: images, toolkits, blueprints, shim
	CatalogArtifacts                // run outputs + workbench snapshots
)

var (
	// ErrImmutableAlias is returned when repointing an exact alias (ADR-007 E2).
	ErrImmutableAlias = errors.New("warehouse: exact alias is immutable (cannot repoint)")
	// ErrUnknownRef is returned when a name/channel does not resolve.
	ErrUnknownRef = errors.New("warehouse: reference does not resolve")
)

// entry records a catalog member (metadata; the bytes live in the CAS).
type entry struct {
	Digest  *arkwenv1.Digest
	Catalog Catalog
	Name    string
}

// Warehouse is the catalog + channel index over a shared CAS store.
type Warehouse struct {
	cas cas.Store

	mu       sync.RWMutex
	entries  map[string]entry            // hex -> entry (every blob the warehouse knows)
	aliases  map[string]*arkwenv1.Digest // immutable exact alias -> digest
	channels map[string]*arkwenv1.Digest // movable channel -> digest
	ledger   *Ledger
}

// New builds a Warehouse over the given CAS store.
func New(store cas.Store) *Warehouse {
	return &Warehouse{
		cas:      store,
		entries:  map[string]entry{},
		aliases:  map[string]*arkwenv1.Digest{},
		channels: map[string]*arkwenv1.Digest{},
		ledger:   NewLedger(),
	}
}

// Ledger returns the Warehouse provenance ledger (its own truth domain).
func (w *Warehouse) Ledger() *Ledger { return w.ledger }

// Put stores bytes in a catalog and records the entry. Idempotent by content.
func (w *Warehouse) Put(ctx context.Context, cat Catalog, name string, data []byte, mime string) (*arkwenv1.Digest, error) {
	ref, err := w.cas.Put(ctx, name, data, mime)
	if err != nil {
		return nil, err
	}
	d := ref.GetContentHash()
	w.mu.Lock()
	w.entries[d.GetHex()] = entry{Digest: d, Catalog: cat, Name: name}
	w.mu.Unlock()
	return d, nil
}

// SetAlias binds an immutable exact alias to a digest. Rebinding to a DIFFERENT
// digest fails (immutable); rebinding to the same digest is a no-op.
func (w *Warehouse) SetAlias(alias string, d *arkwenv1.Digest) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if cur, ok := w.aliases[alias]; ok && cur.GetHex() != d.GetHex() {
		return fmt.Errorf("%w: %s", ErrImmutableAlias, alias)
	}
	w.aliases[alias] = d
	return nil
}

// MoveChannel repoints a movable channel (dev/tested/released) to a digest and
// records a ChannelPointerMoved in the ledger (ADR-007 E2). Channels — unlike
// exact aliases — are meant to move.
func (w *Warehouse) MoveChannel(channel string, to *arkwenv1.Digest, by *arkwenv1.Principal) (uint64, error) {
	w.mu.Lock()
	from := w.channels[channel]
	w.channels[channel] = to
	w.mu.Unlock()
	return w.ledger.RecordChannelMove(by, channel, from, to), nil
}

// Resolve resolves a name/alias/channel to its digest EXACTLY ONCE (the caller
// freezes the result into the seed). A bare 64-hex digest resolves to itself.
func (w *Warehouse) Resolve(nameOrChannel string) (*arkwenv1.Digest, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if d, ok := w.aliases[nameOrChannel]; ok {
		return d, nil
	}
	if d, ok := w.channels[nameOrChannel]; ok {
		return d, nil
	}
	return nil, fmt.Errorf("%w: %q", ErrUnknownRef, nameOrChannel)
}

// Has reports whether the warehouse knows a blob by digest.
func (w *Warehouse) Has(d *arkwenv1.Digest) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	_, ok := w.entries[d.GetHex()]
	return ok
}

// storedDigests returns every blob digest the warehouse knows.
func (w *Warehouse) storedDigests() []*arkwenv1.Digest {
	w.mu.RLock()
	defer w.mu.RUnlock()
	out := make([]*arkwenv1.Digest, 0, len(w.entries))
	for _, e := range w.entries {
		out = append(out, e.Digest)
	}
	return out
}

// channelPointers returns the current channel targets (GC roots).
func (w *Warehouse) channelPointers() []*arkwenv1.Digest {
	w.mu.RLock()
	defer w.mu.RUnlock()
	out := make([]*arkwenv1.Digest, 0, len(w.channels))
	for _, d := range w.channels {
		out = append(out, d)
	}
	return out
}
