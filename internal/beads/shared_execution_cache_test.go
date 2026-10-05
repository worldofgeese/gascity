package beads

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

type executionCapabilityHidden struct{ Store }

func TestSharedExecutionCapabilityRefusesLegacyAndUnsupportedWrappers(t *testing.T) {
	for _, store := range []Store{nil, &executionCapabilityHidden{NewMemStore()}, NewCachingStoreForTest(&executionCapabilityHidden{NewMemStore()}, nil)} {
		if _, err := ResolveSharedExecutionStore(store); !errors.Is(err, ErrSharedExecutionUnsupported) {
			t.Fatalf("store %T advertised unproved execution safety: %v", store, err)
		}
	}
	if IsExecutionOwned(Bead{ID: "task-1", Assignee: "exec-worker"}) {
		t.Fatal("a legacy display name was mistaken for a grant")
	}
	if !IsExecutionOwned(Bead{ID: "task-1", Metadata: map[string]string{ExecutionIDKey: "damaged"}}) {
		t.Fatal("damaged explicit shared marker was treated as legacy")
	}
}

func TestSharedExecutionCachingAuthorityInvalidatesLeaseAndTerminalWrites(t *testing.T) {
	work := NewMemStore()
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	work.executionNow = func() time.Time { return now }
	b, _ := work.Create(Bead{Title: "Human task"})
	cache := NewCachingStoreForTest(work, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	authority, err := ResolveSharedExecutionStore(cache)
	if err != nil {
		t.Fatal(err)
	}
	grant, won, err := authority.AcquireExecution(b.ID, ExecutionOwner{CityID: "a"}, time.Minute)
	if err != nil || !won {
		t.Fatalf("cached acquire: %v, %v", won, err)
	}
	cached, err := cache.Get(b.ID)
	if err != nil || cached.Assignee != grant.ID {
		t.Fatalf("cache retained unassigned row: %+v, %v", cached, err)
	}
	now = now.Add(30 * time.Second)
	renewed, err := authority.MutateExecution(grant, ExecutionMutation{Operation: ExecutionRenew})
	if err != nil {
		t.Fatal(err)
	}
	cached, err = cache.Get(b.ID)
	if err != nil || cached.Metadata[executionDeadlineKey] != renewed.Metadata[executionDeadlineKey] {
		t.Fatalf("cache retained stale lease: %+v, %v", cached, err)
	}
	if _, err := authority.MutateExecution(grant, ExecutionMutation{Operation: ExecutionRelease}); err != nil {
		t.Fatal(err)
	}
	cached, err = cache.Get(b.ID)
	if err != nil || cached.Assignee != "" || cached.Status != "open" {
		t.Fatalf("cache retained old owner: %+v, %v", cached, err)
	}
	current, _, _ := authority.AcquireExecution(b.ID, ExecutionOwner{CityID: "b"}, time.Minute)
	if _, err := authority.MutateExecution(grant, ExecutionMutation{Operation: ExecutionComplete}); !errors.Is(err, ErrExecutionLost) {
		t.Fatalf("stale cached completion = %v", err)
	}
	cached, err = cache.Get(b.ID)
	if err != nil || cached.Assignee != current.ID {
		t.Fatalf("cache lost successor after conflict: %+v, %v", cached, err)
	}
	if _, err := authority.MutateExecution(current, ExecutionMutation{Operation: ExecutionComplete}); err != nil {
		t.Fatal(err)
	}
	cached, err = cache.Get(b.ID)
	if err != nil || cached.Status != "closed" || cached.Assignee != "" {
		t.Fatalf("cache retained completed owner: %+v, %v", cached, err)
	}
}

func TestSharedExecutionFileSaveFailureRollsBackWholeTransition(t *testing.T) {
	fs := fsys.NewFake()
	work, err := OpenFileStore(fs, "city/work.json")
	if err != nil {
		t.Fatal(err)
	}
	b, err := work.Create(Bead{Title: "Human task", Metadata: map[string]string{"human": "keep"}})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := work.Get(b.ID)
	injected := errors.New("disk unavailable")
	fs.Errors["city/work.json.tmp"] = injected
	grant, won, err := work.AcquireExecution(b.ID, ExecutionOwner{CityID: "a"}, time.Minute)
	if !errors.Is(err, injected) || won || grant.ID != "" {
		t.Fatalf("failed save reported a winner: %+v, %v, %v", grant, won, err)
	}
	after, _ := work.MemStore.Get(b.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("acquire save failure changed memory: %+v", after)
	}
	delete(fs.Errors, "city/work.json.tmp")
	grant, won, err = work.AcquireExecution(b.ID, ExecutionOwner{CityID: "a"}, time.Minute)
	if err != nil || !won {
		t.Fatalf("acquire after disk repair: %v %v", won, err)
	}
	before, _ = work.InspectExecution(grant)
	fs.Errors["city/work.json.tmp"] = injected
	if _, err := work.MutateExecution(grant, ExecutionMutation{Operation: ExecutionComplete, AppendComment: "not committed"}); !errors.Is(err, injected) {
		t.Fatalf("terminal save error = %v", err)
	}
	after, _ = work.MemStore.Get(b.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("failed terminal save partially cleared claim/content: %+v", after)
	}
}
