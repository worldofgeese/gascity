package beads_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
)

func isolatedDeleteStore(t *testing.T, backend string) beads.Store {
	t.Helper()
	var store beads.Store
	switch strings.TrimPrefix(backend, "cache/") {
	case "memory":
		store = beads.NewMemStore()
	case "file":
		var err error
		store, err = beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
		if err != nil {
			t.Fatal(err)
		}
	case "sqlite":
		store = newSQLiteForConformance(t)
	default:
		t.Fatalf("unknown backend %q", backend)
	}
	if strings.HasPrefix(backend, "cache/") {
		cache := beads.NewCachingStoreForTest(store, nil)
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatal(err)
		}
		return cache
	}
	return store
}

func isolatedRowsJSON(t *testing.T, store beads.Store) string {
	t.Helper()
	rows, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth, Live: true})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestIsolatedDeleteConformance(t *testing.T) {
	for _, backend := range []string{"memory", "file", "sqlite", "cache/memory", "cache/file", "cache/sqlite"} {
		for _, shape := range []string{
			"isolated", "isolated closed", "stale revision", "parent", "attachment",
			"closed child", "closed member", "parent metadata", "source metadata", "convoy metadata",
			"incoming dependency", "outgoing dependency",
		} {
			t.Run(backend+"/"+shape, func(t *testing.T) {
				store := isolatedDeleteStore(t, backend)
				deleter, ok := beads.IsolatedDeleterFor(store)
				if !ok {
					t.Fatal("local backend lacks isolated deletion")
				}
				root, err := store.Create(beads.Bead{Title: "root", Type: "task", Ephemeral: true})
				if err != nil {
					t.Fatal(err)
				}
				if err := store.SetMetadata(root.ID, beadmeta.RootBeadIDMetadataKey, root.ID); err != nil {
					t.Fatal(err)
				}
				root, err = store.Get(root.ID)
				if err != nil {
					t.Fatal(err)
				}
				stale := root.Revision
				other, err := store.Create(beads.Bead{Title: "unrelated work", Type: "task"})
				if err != nil {
					t.Fatal(err)
				}
				if err := store.DepAdd(other.ID, "foreign-work", "related"); err != nil {
					t.Fatal(err)
				}
				switch shape {
				case "isolated closed":
					err = store.Close(root.ID)
				case "stale revision":
					title := "changed root"
					err = store.Update(root.ID, beads.UpdateOpts{Title: &title})
				case "parent":
					parent := other.ID
					err = store.Update(root.ID, beads.UpdateOpts{ParentID: &parent})
				case "attachment":
					err = store.SetMetadata(root.ID, beadmeta.SourceBeadIDMetadataKey, other.ID)
				case "closed child":
					_, err = store.Create(beads.Bead{Title: "child", ParentID: root.ID, Status: "closed"})
				case "closed member", "parent metadata", "source metadata", "convoy metadata":
					key := map[string]string{
						"closed member":   beadmeta.RootBeadIDMetadataKey,
						"parent metadata": beadmeta.ParentBeadIDMetadataKey,
						"source metadata": beadmeta.SourceBeadIDMetadataKey,
						"convoy metadata": beadmeta.InputConvoyIDMetadataKey,
					}[shape]
					_, err = store.Create(beads.Bead{
						Title: "member", Status: "closed", NoHistory: true,
						Metadata: map[string]string{key: root.ID},
					})
				case "incoming dependency":
					err = store.DepAdd(other.ID, root.ID, "blocks")
				case "outgoing dependency":
					err = store.DepAdd(root.ID, other.ID, "related")
				}
				if err != nil {
					t.Fatal(err)
				}
				root, err = store.Get(root.ID)
				if err != nil {
					t.Fatal(err)
				}
				revision := root.Revision
				if shape == "stale revision" {
					revision = stale
				}
				before := isolatedRowsJSON(t, store)
				err = deleter.DeleteIsolatedIfMatch(root.ID, revision)
				switch {
				case strings.HasPrefix(shape, "isolated"):
					if err != nil {
						t.Fatal(err)
					}
					if _, err := store.Get(root.ID); !errors.Is(err, beads.ErrNotFound) {
						t.Fatalf("target was not deleted: %v", err)
					}
					if _, err := store.Get(other.ID); err != nil {
						t.Fatalf("unrelated work changed: %v", err)
					}
				case shape == "stale revision":
					if !beads.IsPreconditionFailed(err) {
						t.Fatalf("stale revision = %v", err)
					}
				default:
					if !errors.Is(err, beads.ErrNotIsolated) {
						t.Fatalf("unsupported shape = %v", err)
					}
				}
				if !strings.HasPrefix(shape, "isolated") && isolatedRowsJSON(t, store) != before {
					t.Fatal("refused deletion changed rows")
				}
				deps, err := store.DepList(other.ID, "down")
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, dep := range deps {
					found = found || dep.DependsOnID == "foreign-work"
				}
				if !found {
					t.Fatal("deletion removed an unrelated edge")
				}
			})
		}
	}
}

type isolatedConditionalOnly struct {
	beads.Store
	beads.ConditionalWriter
}

func TestIsolatedDeleteCapabilityIsNotConditionalDelete(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{Title: "root"})
	if err != nil {
		t.Fatal(err)
	}
	wrapped := isolatedConditionalOnly{Store: store, ConditionalWriter: store}
	if _, ok := beads.IsolatedDeleterFor(wrapped); ok {
		t.Fatal("ordinary conditional deletion was mistaken for atomic isolation")
	}
	cache := beads.NewCachingStoreForTest(wrapped, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := cache.DeleteIsolatedIfMatch(root.ID, root.Revision); !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
		t.Fatalf("capability-absent cache = %v", err)
	}
	if _, err := store.Get(root.ID); err != nil {
		t.Fatalf("unsupported backend changed the root: %v", err)
	}
	if _, ok := beads.IsolatedDeleterFor(beads.GraphStore{Store: cache}); !ok {
		t.Fatal("declared class wrapper hid the cache's forwarding method")
	}
	if _, ok := beads.IsolatedDeleterFor(nil); ok {
		t.Fatal("nil store has isolated deletion")
	}
}

func TestIsolatedDeleteFileReloadAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")
	open := func() *beads.FileStore {
		t.Helper()
		store, err := beads.OpenFileStore(fsys.OSFS{}, path)
		if err != nil {
			t.Fatal(err)
		}
		return store
	}
	first := open()
	root, err := first.Create(beads.Bead{Title: "root"})
	if err != nil {
		t.Fatal(err)
	}
	second := open()
	child, err := second.Create(beads.Bead{Title: "late child", ParentID: root.ID, Status: "closed"})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.DeleteIsolatedIfMatch(root.ID, root.Revision); !errors.Is(err, beads.ErrNotIsolated) {
		t.Fatalf("stale file handle missed a child: %v", err)
	}
	if err := second.Delete(child.ID); err != nil {
		t.Fatal(err)
	}
	if err := first.DeleteIsolatedIfMatch(root.ID, root.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := open().Get(root.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("isolated deletion did not persist: %v", err)
	}
}
