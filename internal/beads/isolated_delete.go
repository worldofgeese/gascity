package beads

import (
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// ErrNotIsolated means an exact-target deletion would leave graph references.
var ErrNotIsolated = errors.New("bead is not isolated")

// IsolatedDeleter deletes one revision-matching row only when it has no parent,
// graph members, attachments, or incoming/outgoing dependencies. The revision
// and isolation checks share the deletion's lock or transaction, across both
// tiers and all statuses. It never cascades or removes another row's edges.
//
// ConditionalWriter alone cannot implement this: adding a child or dependency
// need not change the target revision.
type IsolatedDeleter interface {
	DeleteIsolatedIfMatch(id string, expectedRevision int64) error
}

var (
	_ IsolatedDeleter = (*MemStore)(nil)
	_ IsolatedDeleter = (*FileStore)(nil)
	_ IsolatedDeleter = (*SQLiteStore)(nil)
	_ IsolatedDeleter = (*CachingStore)(nil)
)

// IsolatedDeleterFor is a hard capability lookup, with no unfenced fallback.
// Declared wrapper targets are followed; caches must retain their own handle.
func IsolatedDeleterFor(store Store) (IsolatedDeleter, bool) {
	if store == nil {
		return nil, false
	}
	deleter, ok := followConditionalWritesResolveTarget(store).(IsolatedDeleter)
	return deleter, ok
}

var isolatedGraphReferenceKeys = []string{
	beadmeta.RootBeadIDMetadataKey,
	beadmeta.ParentBeadIDMetadataKey,
	beadmeta.SourceBeadIDMetadataKey,
	beadmeta.InputConvoyIDMetadataKey,
}

func validateIsolatedDeleteTarget(b Bead) error {
	if b.ParentID != "" || len(b.Needs) != 0 || len(b.Dependencies) != 0 {
		return fmt.Errorf("deleting bead %q: %w", b.ID, ErrNotIsolated)
	}
	for _, key := range isolatedGraphReferenceKeys {
		if value := b.Metadata[key]; value != "" &&
			(key != beadmeta.RootBeadIDMetadataKey || value != b.ID) {
			return fmt.Errorf("deleting bead %q: %w (%s)", b.ID, ErrNotIsolated, key)
		}
	}
	return nil
}
