package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestSharedExecutionCreateRequiresOriginalGrant(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		store := beads.NewMemStore()
		provider := runtime.NewFake()
		manager := NewManagerWithOptions(store, provider)
		_, err := manager.CreateSession(context.Background(), CreateOptions{
			Template: "worker", Command: "fixture-agent", Provider: "fixture", WorkDir: t.TempDir(),
			BeadOnly: deferred,
			ExtraMeta: map[string]string{
				"session_origin": "shared-execution", "shared_work_id": "team-1",
				"shared_execution_id": "exec-" + strings.Repeat("a", 64),
			},
		})
		if !errors.Is(err, beads.ErrExecutionRequired) {
			t.Fatalf("shared create without bound authority = %v, deferred=%v", err, deferred)
		}
		rows, listErr := NewStore(beads.SessionStore{Store: store}).ListAll(ListAllOptions{IncludeClosed: true})
		if listErr != nil || len(rows) != 0 || len(provider.SnapshotCalls()) != 0 {
			t.Fatalf("refused create wrote or launched: %+v, %+v", rows, provider.Calls)
		}
	}
}

func TestSharedExecutionCreateRefusesMissingScopeBeforeAdmission(t *testing.T) {
	for _, scope := range []string{"", " ", "team "} {
		t.Run("scope="+scope, func(t *testing.T) {
			work := beads.NewMemStore()
			task, err := work.Create(beads.Bead{Title: "Human task"})
			if err != nil {
				t.Fatal(err)
			}
			grant, won, err := work.AcquireExecution(task.ID, beads.ExecutionOwner{CityID: "city"}, time.Minute)
			if err != nil || !won {
				t.Fatalf("acquire: %v, %v", won, err)
			}
			meta, err := SharedExecutionMetadata(grant, "team")
			if err != nil {
				t.Fatal(err)
			}
			meta[sharedWorkScopeMetadataKey] = scope
			store, provider := beads.NewMemStore(), runtime.NewFake()
			manager := NewManagerWithOptions(store, provider)
			_, err = manager.CreateSession(WithSharedExecutionStart(context.Background(), grant, work), CreateOptions{
				Template: "worker", Command: "fixture-agent", Provider: "fixture", WorkDir: t.TempDir(), ExtraMeta: meta,
			})
			if !errors.Is(err, beads.ErrExecutionRequired) {
				t.Fatalf("incomplete original scope authorized launch: %v", err)
			}
			rows, listErr := NewStore(beads.SessionStore{Store: store}).ListAll(ListAllOptions{IncludeClosed: true})
			if listErr != nil || len(rows) != 0 || len(provider.SnapshotCalls()) != 0 {
				t.Fatal("invalid scope wrote a session or launched a runtime")
			}
			if _, err := SharedExecutionMetadata(grant, scope); !errors.Is(err, beads.ErrExecutionRequired) {
				t.Fatalf("binding helper accepted invalid scope: %v", err)
			}
		})
	}
}
