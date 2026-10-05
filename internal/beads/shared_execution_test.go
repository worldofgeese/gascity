package beads

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestSharedExecutionAuthorityLifecycle(t *testing.T) {
	for _, backend := range []string{"memory", "file"} {
		t.Run(backend, func(t *testing.T) {
			clk := &clock.Fake{Time: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
			var store Store
			if backend == "memory" {
				mem := NewMemStore()
				mem.executionNow = clk.Now
				store = mem
			} else {
				file, err := OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "work.json"), WithFileStoreExecutionClock(clk.Now))
				if err != nil {
					t.Fatal(err)
				}
				store = file
			}
			authority, err := ResolveSharedExecutionStore(store)
			if err != nil {
				t.Fatal(err)
			}
			work, err := store.Create(Bead{
				Title: "A human's unlabelled task", Description: "Original human notes",
				Metadata: map[string]string{
					"human": "keep", ExecutionCommentsKey: `[{"text":"human note","author":"Ada","custom":{"keep":true}}]`,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			owner := ExecutionOwner{CityID: "a", DisplayName: "same-name"}
			old, won, err := authority.AcquireExecution(work.ID, owner, time.Minute)
			if err != nil || !won {
				t.Fatalf("acquire = %+v, %v, %v", old, won, err)
			}
			started, err := authority.MutateExecution(old, ExecutionMutation{
				Operation: ExecutionStart, SessionID: "city-a-session", SessionName: "runtime-a",
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := authority.MutateExecution(old, ExecutionMutation{
				Operation: ExecutionStart, SessionID: "city-a-session", SessionName: "runtime-a",
			}); !errors.Is(err, ErrExecutionAlreadyStarted) {
				t.Fatalf("launch replay = %v", err)
			}
			clk.Advance(59 * time.Second)
			renewed, err := authority.MutateExecution(old, ExecutionMutation{Operation: ExecutionRenew})
			if err != nil || renewed.Revision != started.Revision {
				t.Fatalf("lease renewal changes no issue revision: %+v, %v", renewed, err)
			}
			clk.Advance(time.Second)
			if _, reclaimed, err := authority.ReclaimExecution(work.ID); err != nil || reclaimed {
				t.Fatalf("renewed live execution reclaimed: %v, %v", reclaimed, err)
			}
			clk.Advance(59 * time.Second)
			released, reclaimed, err := authority.ReclaimExecution(work.ID)
			if err != nil || !reclaimed {
				t.Fatalf("exact deadline reclaim = %v, %v", reclaimed, err)
			}
			assertExecutionCleaned(t, released, "open")
			successor, won, err := authority.AcquireExecution(work.ID, ExecutionOwner{CityID: "b", DisplayName: "same-name"}, time.Minute)
			if err != nil || !won || successor.ID == old.ID {
				t.Fatalf("fresh takeover = %+v, %v, %v", successor, won, err)
			}
			before, err := authority.MutateExecution(successor, ExecutionMutation{
				Operation: ExecutionStart, SessionID: "city-b-session", SessionName: "runtime-b",
			})
			if err != nil {
				t.Fatal(err)
			}
			before, err = authority.InspectExecution(successor)
			if err != nil {
				t.Fatal(err)
			}
			for _, operation := range []ExecutionOperation{ExecutionUpdate, ExecutionComplete, ExecutionRelease, ExecutionRenew, ExecutionStart} {
				mutation := ExecutionMutation{Operation: operation}
				if operation == ExecutionStart {
					mutation.SessionID, mutation.SessionName = "city-a-session", "runtime-a"
				}
				if operation == ExecutionUpdate || operation == ExecutionComplete {
					mutation.AppendComment = "stale note"
					mutation.Update.Metadata = map[string]string{"human": "corrupt"}
				}
				if _, err := authority.MutateExecution(old, mutation); !errors.Is(err, ErrExecutionLost) {
					t.Fatalf("stale %s = %v", operation, err)
				}
				after, err := authority.InspectExecution(successor)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatalf("stale %s changed successor: %+v, %v", operation, after, err)
				}
			}
			completed, err := authority.MutateExecution(successor, ExecutionMutation{
				Operation: ExecutionComplete, AppendComment: "completed by b",
				Update: UpdateOpts{Metadata: map[string]string{"result": "done"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			assertExecutionCleaned(t, completed, "closed")
			if completed.Description != work.Description || completed.Metadata["human"] != "keep" || completed.Metadata["result"] != "done" {
				t.Fatalf("human content lost: %+v", completed)
			}
			var comments []map[string]any
			if err := json.Unmarshal([]byte(completed.Metadata[ExecutionCommentsKey]), &comments); err != nil {
				t.Fatal(err)
			}
			if len(comments) != 2 || comments[0]["text"] != "human note" || comments[0]["author"] != "Ada" || comments[0]["custom"] == nil ||
				comments[1]["execution_id"] != successor.ID || comments[1]["text"] != "completed by b" {
				t.Fatalf("append replaced human comment content: %+v", comments)
			}
			if _, won, err := authority.AcquireExecution(work.ID, owner, time.Minute); err != nil || won {
				t.Fatalf("closed task acquired: %v, %v", won, err)
			}
		})
	}
}

func assertExecutionCleaned(t *testing.T, b Bead, status string) {
	t.Helper()
	if b.Status != status || b.Assignee != "" {
		t.Fatalf("terminal assignment not cleared: %+v", b)
	}
	for key := range executionCleanupMetadata() {
		if b.Metadata[key] != "" {
			t.Fatalf("terminal metadata %q survived: %+v", key, b.Metadata)
		}
	}
}

func TestSharedExecutionExpiredRenewalWinsBeforeCommittedReclaim(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
	path := filepath.Join(t.TempDir(), "work.json")
	first, err := OpenFileStore(fsys.OSFS{}, path, WithFileStoreExecutionClock(clk.Now))
	if err != nil {
		t.Fatal(err)
	}
	second, err := OpenFileStore(fsys.OSFS{}, path, WithFileStoreExecutionClock(clk.Now))
	if err != nil {
		t.Fatal(err)
	}
	work, _ := first.Create(Bead{Title: "Task"})
	grant, _, _ := first.AcquireExecution(work.ID, ExecutionOwner{CityID: "a"}, time.Minute)
	clk.Advance(2 * time.Minute)
	if _, err := first.InspectExecution(grant); err != nil {
		t.Fatalf("expiry alone revoked authority: %v", err)
	}
	if _, err := first.MutateExecution(grant, ExecutionMutation{Operation: ExecutionRenew}); err != nil {
		t.Fatal(err)
	}
	if _, reclaimed, err := second.ReclaimExecution(work.ID); err != nil || reclaimed {
		t.Fatalf("reclaim used stale lease/revision: %v, %v", reclaimed, err)
	}
	clk.Advance(time.Minute)
	if _, reclaimed, err := second.ReclaimExecution(work.ID); err != nil || !reclaimed {
		t.Fatalf("expiry reclaim = %v, %v", reclaimed, err)
	}
	if _, err := first.MutateExecution(grant, ExecutionMutation{Operation: ExecutionRenew}); !errors.Is(err, ErrExecutionLost) {
		t.Fatalf("renew resurrected revoked grant: %v", err)
	}
}

func TestSharedExecutionRefusalsLeaveRowsIntact(t *testing.T) {
	store := NewMemStore()
	work, _ := store.Create(Bead{Title: "Task"})
	grant, _, _ := store.AcquireExecution(work.ID, ExecutionOwner{CityID: "a"}, time.Minute)
	before, _ := store.Get(work.ID)
	assignee, status := "somebody-else", "open"
	description := "replacement human context"
	for _, mutation := range []ExecutionMutation{
		{Operation: "unknown"},
		{Operation: ExecutionStart},
		{Operation: ExecutionUpdate, Update: UpdateOpts{Assignee: &assignee}},
		{Operation: ExecutionUpdate, Update: UpdateOpts{Status: &status}},
		{Operation: ExecutionUpdate, Update: UpdateOpts{Description: &description}},
		{Operation: ExecutionUpdate, Update: UpdateOpts{Metadata: map[string]string{ExecutionIDKey: "borrowed"}}},
		{Operation: ExecutionUpdate, Update: UpdateOpts{Metadata: map[string]string{ExecutionCommentsKey: "replace"}}},
		{Operation: ExecutionRelease, AppendComment: "not a content operation"},
	} {
		if _, err := store.MutateExecution(grant, mutation); err == nil {
			t.Fatalf("unsafe mutation accepted: %+v", mutation)
		}
		after, _ := store.Get(work.ID)
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("refusal changed row: %+v", after)
		}
	}
	for _, invalid := range []ExecutionGrant{{}, {BeadID: work.ID}, {BeadID: work.ID, ID: "exec-bad"}, {BeadID: " " + work.ID, ID: grant.ID}} {
		if _, err := store.MutateExecution(invalid, ExecutionMutation{Operation: ExecutionComplete}); !errors.Is(err, ErrExecutionRequired) {
			t.Fatalf("invalid grant accepted: %+v, %v", invalid, err)
		}
	}
	for _, id := range []string{"", work.ID[:len(work.ID)-1], work.ID + "*"} {
		if _, _, err := store.ReclaimExecution(id); err == nil {
			t.Fatalf("non-exact reclaim accepted: %q", id)
		}
	}
	if err := store.SetMetadata(work.ID, executionDeadlineKey, "broken"); err != nil {
		t.Fatal(err)
	}
	malformed, _ := store.Get(work.ID)
	if _, _, err := store.ReclaimExecution(work.ID); err == nil {
		t.Fatal("malformed lease was reclaimed")
	}
	if _, err := store.MutateExecution(grant, ExecutionMutation{Operation: ExecutionRenew}); err == nil {
		t.Fatal("malformed lease was renewed")
	}
	after, _ := store.Get(work.ID)
	if !reflect.DeepEqual(malformed, after) {
		t.Fatal("malformed lease refusal changed row")
	}
}

func TestSharedExecutionReadinessAndLegacyAssignment(t *testing.T) {
	store := NewMemStore()
	blocker, _ := store.Create(Bead{Title: "Unfinished prerequisite"})
	blocked, _ := store.Create(Bead{Title: "Blocked"})
	if err := store.DepAdd(blocked.ID, blocker.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	assigned, _ := store.Create(Bead{Title: "Legacy owner", Assignee: "legacy-person", Status: "in_progress"})
	closed, _ := store.Create(Bead{Title: "Closed", Status: "closed"})
	inProgress := "in_progress"
	if err := store.Update(assigned.ID, UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(closed.ID); err != nil {
		t.Fatal(err)
	}
	for _, b := range []Bead{blocked, assigned, closed} {
		if _, won, err := store.AcquireExecution(b.ID, ExecutionOwner{CityID: "a"}, time.Minute); err != nil || won {
			t.Fatalf("ineligible work acquired: %s %v %v", b.ID, won, err)
		}
	}
	if _, reclaimed, err := store.ReclaimExecution(assigned.ID); err != nil || reclaimed {
		t.Fatalf("legacy assignee reclaimed: %v %v", reclaimed, err)
	}
	if err := store.Close(blocker.ID); err != nil {
		t.Fatal(err)
	}
	grant, won, err := store.AcquireExecution(blocked.ID, ExecutionOwner{CityID: "a"}, time.Minute)
	if err != nil || !won {
		t.Fatalf("unblocked work not acquired: %v %v", won, err)
	}
	for _, key := range append([]string{beadmeta.WorkDirMetadataKey, beadmeta.WorkBranchMetadataKey}, beadmeta.SessionAffinityMetadataKeys...) {
		if err := store.SetMetadata(blocked.ID, key, "old-execution"); err != nil {
			t.Fatal(err)
		}
	}
	released, err := store.MutateExecution(grant, ExecutionMutation{Operation: ExecutionRelease})
	if err != nil {
		t.Fatal(err)
	}
	assertExecutionCleaned(t, released, "open")
}

func TestSharedExecutionFileAuthoritySingleWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.json")
	first, err := OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	human, err := first.Create(Bead{
		Title: "Human work", Description: "Do not replace these notes",
		Metadata: map[string]string{"human": "preserve", ExecutionCommentsKey: `[{"text":"human context"}]`},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	stores := []Store{first, second}
	var grants [2]ExecutionGrant
	var won [2]bool
	var errs [2]error
	barrier := make(chan struct{})
	var wg sync.WaitGroup
	for i := range stores {
		authority, err := ResolveSharedExecutionStore(stores[i])
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-barrier
			grants[i], won[i], errs[i] = authority.AcquireExecution(human.ID, ExecutionOwner{
				CityID: []string{"city-a", "city-b"}[i], DisplayName: "worker",
			}, time.Minute)
		}(i)
	}
	close(barrier)
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || won[0] == won[1] {
		t.Fatalf("want exactly one winner: won=%v errors=%v", won, errs)
	}
	winner := 0
	if won[1] {
		winner = 1
	}
	authority, _ := ResolveSharedExecutionStore(stores[winner])
	before, err := authority.InspectExecution(grants[winner])
	if err != nil {
		t.Fatal(err)
	}
	if before.Assignee != grants[winner].ID || before.Description != human.Description || before.Metadata["human"] != "preserve" {
		t.Fatalf("incorrect acquired row: %+v", before)
	}
	if grant, ok, err := authority.AcquireExecution(human.ID, ExecutionOwner{CityID: "city-a"}, time.Minute); err != nil || ok || grant.ID != "" {
		t.Fatalf("retry adopted an existing grant: %+v %v %v", grant, ok, err)
	}
	after, err := authority.InspectExecution(grants[winner])
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("loser/retry changed winner: %+v %v", after, err)
	}
}
