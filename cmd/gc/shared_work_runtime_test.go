package main

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

func TestSharedWorkForeignExecutionIsNotALocalOrphan(t *testing.T) {
	work := beads.Bead{
		ID:       "team-1",
		Title:    "Human-created shared work",
		Type:     "task",
		Status:   "in_progress",
		Assignee: "exec-foreign-grant",
		Metadata: map[string]string{
			"gc.execution_id":            "exec-foreign-grant",
			beadmeta.RoutedToMetadataKey: "worker",
			"human-context":              "keep this",
		},
	}
	shared := beads.NewMemStoreFrom(1, []beads.Bead{work}, nil)
	localSessions := beads.NewMemStore()
	released := releaseOrphanedPoolAssignments(
		shared, beads.SessionStore{Store: localSessions}, testPoolReleaseConfig(),
		"", nil, []beads.Bead{work}, []beads.Store{shared}, nil, nil, nil, nil,
	)
	if len(released) != 0 {
		t.Fatalf("foreign execution released merely because its session is absent locally: %v", released)
	}
	after, err := shared.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(work, after) {
		t.Fatalf("foreign claim changed: before=%+v after=%+v", work, after)
	}
}
