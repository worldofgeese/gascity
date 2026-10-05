package molecule

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storebinding"
)

func rootWispStore(t *testing.T) (*beads.MemStore, storebinding.GraphStore) {
	t.Helper()
	store := beads.NewMemStore()
	graph, err := storebinding.NewBeadsGraphStore(store)
	if err != nil {
		t.Fatal(err)
	}
	return store, graph
}

func seedRootWisp(t *testing.T, store beads.Store, title, kind string) beads.Bead {
	t.Helper()
	b := beads.Bead{
		Title: title, Type: "task", Ref: "loop", Assignee: "seat-1",
		Ephemeral: kind == beadmeta.KindWisp, NoHistory: kind == beadmeta.KindWorkflow,
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:        kind,
			beadmeta.FormulaNameMetadataKey: "loop",
		},
	}
	if kind == beadmeta.KindWorkflow {
		b.Metadata[beadmeta.FormulaContractMetadataKey] = beadmeta.FormulaContractGraphV2
	}
	got, err := store.Create(b)
	if err != nil {
		t.Fatal(err)
	}
	if kind == beadmeta.KindWorkflow {
		if err := store.SetMetadata(got.ID, beadmeta.RootBeadIDMetadataKey, got.ID); err != nil {
			t.Fatal(err)
		}
	}
	got, err = store.Get(got.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestRootWispListExactActiveExecution(t *testing.T) {
	store, graph := rootWispStore(t)
	first := seedRootWisp(t, store, "local-1", beadmeta.KindWisp)
	second := seedRootWisp(t, store, "local-2", beadmeta.KindWorkflow)
	if err := store.Update(second.ID, beads.UpdateOpts{Status: strp("in_progress")}); err != nil {
		t.Fatal(err)
	}
	closed := seedRootWisp(t, store, "local-3", beadmeta.KindWisp)
	if err := store.Close(closed.ID); err != nil {
		t.Fatal(err)
	}
	other := seedRootWisp(t, store, "local-4", beadmeta.KindWisp)
	if err := store.Update(other.ID, beads.UpdateOpts{Assignee: strp("seat-2")}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(beads.Bead{ID: "work-1", Type: "task", Assignee: "seat-1"}); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		got, err := ListRootWisps(graph, "loop", "seat-1")
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(got))
		for _, b := range got {
			ids = append(ids, b.ID)
		}
		if want := []string{first.ID, second.ID}; !reflect.DeepEqual(ids, want) {
			t.Fatalf("root ids = %v, want %v", ids, want)
		}
	}
	got, err := ListRootWisps(graph, "another-loop", "seat-1")
	if err != nil || len(got) != 0 {
		t.Fatalf("other formula = (%v, %v), want empty", got, err)
	}
}

func TestRootWispListFormulaIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, ref, step string
		valid           bool
	}{
		{"sequential root", "loop", "", true},
		{"batch root", "", "loop", true},
		{"matching identities", "loop", "loop", true},
		{"conflicting ref", "another-loop", "loop", false},
		{"conflicting step", "loop", "loop.child", false},
		{"no root identity", "", "", false},
		{"child identity", "", "loop.child", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, graph := rootWispStore(t)
			root, err := store.Create(beads.Bead{
				Title: "root", Type: "task", Ref: tc.ref, Assignee: "seat-1",
				Metadata: map[string]string{
					beadmeta.KindMetadataKey:        beadmeta.KindWisp,
					beadmeta.FormulaNameMetadataKey: "loop",
					beadmeta.StepIDMetadataKey:      tc.step,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			roots, err := ListRootWisps(graph, "loop", "seat-1")
			if tc.valid {
				if err != nil || len(roots) != 1 || roots[0].ID != root.ID {
					t.Fatalf("valid root identity not found: %v, %v", roots, err)
				}
			} else if err == nil {
				t.Fatalf("unsupported root identity was accepted: %v", roots)
			}
		})
	}
}

func TestRootWispAssignUnassignedAndNeverSteal(t *testing.T) {
	store, graph := rootWispStore(t)
	root := seedRootWisp(t, store, "local-1", beadmeta.KindWisp)
	requireWispUpdate(t, store, root.ID, beads.UpdateOpts{Assignee: strp("")})
	if err := AssignRootWisp(graph, root.ID, "loop", "seat-1"); err != nil {
		t.Fatal(err)
	}
	assigned, err := store.Get(root.ID)
	if err != nil || assigned.Assignee != "seat-1" {
		t.Fatalf("assignment = %+v, %v", assigned, err)
	}
	for _, assignee := range []string{"seat-1", "seat-2"} {
		err := AssignRootWisp(graph, root.ID, "loop", assignee)
		if (err == nil) != (assignee == "seat-1") {
			t.Fatalf("assign to %q = %v", assignee, err)
		}
		got, err := store.Get(root.ID)
		if err != nil || !reflect.DeepEqual(got, assigned) {
			t.Fatalf("repeat or refused assignment changed root: %+v, %v", got, err)
		}
	}
	if err := store.Close(root.ID); err != nil {
		t.Fatal(err)
	}
	closed, err := store.Get(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := AssignRootWisp(graph, root.ID, "loop", "seat-1"); err == nil {
		t.Fatal("closed root assignment succeeded")
	}
	if got, err := store.Get(root.ID); err != nil || !reflect.DeepEqual(got, closed) {
		t.Fatalf("refused assignment changed closed root: %+v, %v", got, err)
	}
}

func TestRootWispBurnOnlyExactTargetWithoutDigest(t *testing.T) {
	for _, kind := range []string{beadmeta.KindWisp, beadmeta.KindWorkflow} {
		t.Run(kind, func(t *testing.T) {
			store, graph := rootWispStore(t)
			root := seedRootWisp(t, store, "local-1", kind)
			next := seedRootWisp(t, store, "local-2", kind)
			work, err := store.Create(beads.Bead{ID: "work-1", Type: "task", Title: "shared work"})
			if err != nil {
				t.Fatal(err)
			}
			foreign, _ := rootWispStore(t)
			twin := seedRootWisp(t, foreign, root.ID, kind)

			if err := BurnRootWisp(graph, root.ID, "loop", "seat-1", false); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Get(root.ID); !errors.Is(err, beads.ErrNotFound) {
				t.Fatalf("burned root still present: %v", err)
			}
			for _, want := range []beads.Bead{next, work} {
				got, err := store.Get(want.ID)
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("unrelated row changed: got %+v, %v; want %+v", got, err, want)
				}
			}
			got, err := foreign.Get(root.ID)
			if err != nil || !reflect.DeepEqual(got, twin) {
				t.Fatalf("foreign-store twin changed: %+v, %v", got, err)
			}
			all, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
			if err != nil || len(all) != 2 {
				t.Fatalf("burn created a digest or retained a closed root: %v, %v", all, err)
			}
		})
	}
}

func TestRootWispRefusesUnsupportedShapes(t *testing.T) {
	cases := map[string]func(*testing.T, *beads.MemStore, beads.Bead){
		"ordinary work": func(t *testing.T, s *beads.MemStore, b beads.Bead) {
			t.Helper()
			requireWispUpdate(t, s, b.ID, beads.UpdateOpts{Metadata: map[string]string{beadmeta.KindMetadataKey: "task"}})
		},
		"expanded workflow": func(t *testing.T, s *beads.MemStore, b beads.Bead) {
			t.Helper()
			requireWispUpdate(t, s, b.ID, beads.UpdateOpts{Metadata: map[string]string{beadmeta.WorkflowExpandedMetadataKey: "true"}})
		},
		"unfinished materialization": func(t *testing.T, s *beads.MemStore, b beads.Bead) {
			t.Helper()
			requireWispUpdate(t, s, b.ID, beads.UpdateOpts{Metadata: map[string]string{beadmeta.InstantiatingMetadataKey: "true"}})
		},
		"attached root": func(t *testing.T, s *beads.MemStore, b beads.Bead) {
			t.Helper()
			requireWispUpdate(t, s, b.ID, beads.UpdateOpts{Metadata: map[string]string{beadmeta.SourceBeadIDMetadataKey: "work-1"}})
		},
		"input convoy": func(t *testing.T, s *beads.MemStore, b beads.Bead) {
			t.Helper()
			requireWispUpdate(t, s, b.ID, beads.UpdateOpts{Metadata: map[string]string{beadmeta.InputConvoyIDMetadataKey: "work-1"}})
		},
		"another root": func(t *testing.T, s *beads.MemStore, b beads.Bead) {
			t.Helper()
			requireWispUpdate(t, s, b.ID, beads.UpdateOpts{Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "other-1"}})
		},
		"child by parent": func(t *testing.T, s *beads.MemStore, b beads.Bead) {
			t.Helper()
			if _, err := s.Create(beads.Bead{ID: "child-1", ParentID: b.ID}); err != nil {
				t.Fatal(err)
			}
		},
		"closed graph child": func(t *testing.T, s *beads.MemStore, b beads.Bead) {
			t.Helper()
			if _, err := s.Create(beads.Bead{ID: "child-1", Status: "closed", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: b.ID}}); err != nil {
				t.Fatal(err)
			}
		},
		"incoming dependency": func(t *testing.T, s *beads.MemStore, b beads.Bead) {
			t.Helper()
			if err := s.DepAdd("work-1", b.ID, "blocks"); err != nil {
				t.Fatal(err)
			}
		},
		"outgoing dependency": func(t *testing.T, s *beads.MemStore, b beads.Bead) {
			t.Helper()
			if err := s.DepAdd(b.ID, "work-1", "related"); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			store, graph := rootWispStore(t)
			root := seedRootWisp(t, store, "local-1", beadmeta.KindWorkflow)
			mutate(t, store, root)
			before, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ListRootWisps(graph, "loop", "seat-1"); err == nil {
				t.Fatal("lookup hid an unsupported existing root instead of refusing")
			}
			if err := AssignRootWisp(graph, root.ID, "loop", "seat-1"); err == nil {
				t.Fatal("unsupported shape was assigned")
			}
			if err := BurnRootWisp(graph, root.ID, "loop", "seat-1", false); err == nil {
				t.Fatal("unsupported shape was burned")
			}
			after, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("refused burn changed rows: %v", err)
			}
		})
	}
}

func requireWispUpdate(t *testing.T, store beads.Store, id string, opts beads.UpdateOpts) {
	t.Helper()
	if err := store.Update(id, opts); err != nil {
		t.Fatal(err)
	}
}

func TestRootWispRequiresExactSelectors(t *testing.T) {
	store, graph := rootWispStore(t)
	root := seedRootWisp(t, store, "local-123", beadmeta.KindWisp)
	for _, args := range [][3]string{
		{"local-1", "loop", "seat-1"},
		{root.ID, "other-loop", "seat-1"},
		{root.ID, "loop", "seat-2"},
		{root.ID, "", "seat-1"},
		{root.ID, "loop", ""},
		{"", "loop", "seat-1"},
	} {
		if err := AssignRootWisp(graph, args[0], args[1], args[2]); err == nil {
			t.Fatalf("invalid assignment selectors accepted: %q", args)
		}
		if err := BurnRootWisp(graph, args[0], args[1], args[2], false); err == nil {
			t.Fatalf("invalid selectors accepted: %q", args)
		}
	}
	if got, err := store.Get(root.ID); err != nil || !reflect.DeepEqual(got, root) {
		t.Fatalf("invalid selectors changed target: %+v, %v", got, err)
	}
}

type rootWispFaultGraph struct {
	storebinding.GraphStore
	listErr   error
	depErr    error
	deleteErr error
	before    func()
	noDelete  bool
	superset  []beads.Bead
	queries   *[]beads.ListQuery
}

func (s rootWispFaultGraph) List(q beads.ListQuery) ([]beads.Bead, error) {
	if s.queries != nil {
		*s.queries = append(*s.queries, q)
	}
	if s.listErr != nil {
		return nil, s.listErr
	}
	if s.superset != nil {
		return s.superset, nil
	}
	return s.GraphStore.List(q)
}

func (s rootWispFaultGraph) DepList(id, direction string) ([]beads.Dep, error) {
	if s.depErr != nil {
		return nil, s.depErr
	}
	return s.GraphStore.DepList(id, direction)
}

func (s rootWispFaultGraph) DeleteIsolatedIfMatch(id string, revision int64) error {
	if s.before != nil {
		s.before()
	}
	if s.deleteErr != nil || s.noDelete {
		return s.deleteErr
	}
	return s.GraphStore.DeleteIsolatedIfMatch(id, revision)
}

func (s rootWispFaultGraph) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	if s.before != nil {
		s.before()
	}
	return s.GraphStore.UpdateIfMatch(id, revision, opts)
}

func TestRootWispAssignRefusesChangedOrUnsupportedWriter(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unsupported", true: "changed"}[changed], func(t *testing.T) {
			store, graph := rootWispStore(t)
			root := seedRootWisp(t, store, "local-1", beadmeta.KindWisp)
			requireWispUpdate(t, store, root.ID, beads.UpdateOpts{Assignee: strp("")})
			before, err := store.Get(root.ID)
			if err != nil {
				t.Fatal(err)
			}
			fault := rootWispFaultGraph{GraphStore: graph}
			if changed {
				fault.before = func() {
					requireWispUpdate(t, store, root.ID, beads.UpdateOpts{Assignee: strp("seat-2")})
					before, err = store.Get(root.ID)
					if err != nil {
						t.Fatal(err)
					}
				}
			} else {
				store.DisableConditionalWrites = true
			}
			err = AssignRootWisp(fault, root.ID, "loop", "seat-1")
			if changed && !beads.IsPreconditionFailed(err) {
				t.Fatalf("changed assignment = %v, want revision conflict", err)
			}
			if !changed && !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
				t.Fatalf("unsupported writer = %v", err)
			}
			after, err := store.Get(root.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("refused assignment changed root: %+v, %v", after, err)
			}
		})
	}
}

func TestRootWispBurnFailuresAndDryRun(t *testing.T) {
	sentinel := errors.New("store refused")
	for _, phase := range []string{"list", "dependencies", "delete", "unsupported", "changed", "no-op", "dry-run"} {
		t.Run(phase, func(t *testing.T) {
			store, graph := rootWispStore(t)
			root := seedRootWisp(t, store, "local-1", beadmeta.KindWisp)
			fault := rootWispFaultGraph{GraphStore: graph}
			switch phase {
			case "list":
				fault.listErr = sentinel
			case "dependencies":
				fault.depErr = sentinel
			case "delete":
				fault.deleteErr = sentinel
			case "unsupported":
				store.DisableConditionalWrites = true
			case "changed":
				fault.before = func() {
					requireWispUpdate(t, store, root.ID, beads.UpdateOpts{Assignee: strp("seat-2")})
				}
			case "no-op":
				fault.noDelete = true
			}
			err := BurnRootWisp(fault, root.ID, "loop", "seat-1", phase == "dry-run")
			switch phase {
			case "dry-run":
				if err != nil {
					t.Fatal(err)
				}
			case "changed":
				if !beads.IsPreconditionFailed(err) {
					t.Fatalf("changed assignment = %v, want revision conflict", err)
				}
			case "unsupported":
				if !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
					t.Fatalf("unsupported writer = %v", err)
				}
			case "no-op":
				if err == nil || !strings.Contains(err.Error(), "still exists") {
					t.Fatalf("successful no-op delete = %v", err)
				}
			default:
				if !errors.Is(err, sentinel) {
					t.Fatalf("store error lost: %v", err)
				}
			}

			if _, err := store.Get(root.ID); err != nil {
				t.Fatalf("root lost on refused/dry-run burn: %v", err)
			}
		})
	}
}

func TestRootWispUsesLiveExactQueriesAcrossTiers(t *testing.T) {
	store, graph := rootWispStore(t)
	root := seedRootWisp(t, store, "root", beadmeta.KindWisp)
	other := seedRootWisp(t, store, "other", beadmeta.KindWorkflow)
	requireWispUpdate(t, store, other.ID, beads.UpdateOpts{Assignee: strp("seat-2")})
	all, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatal(err)
	}
	var queries []beads.ListQuery
	superset := rootWispFaultGraph{GraphStore: graph, superset: all, queries: &queries}
	found, err := ListRootWisps(superset, "loop", "seat-1")
	if err != nil || len(found) != 1 || found[0].ID != root.ID {
		t.Fatalf("superset lookup = %v, %v", found, err)
	}
	if err := BurnRootWisp(superset, strings.TrimSuffix(root.ID, "1"), "loop", "seat-1", false); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("fuzzy target accepted from provider superset: %v", err)
	}
	if _, err := store.Get(root.ID); err != nil {
		t.Fatal(err)
	}
	for _, query := range queries {
		if !query.Live || query.TierMode != beads.TierBoth || !query.HasFilter() {
			t.Fatalf("lookup was stale, tier-narrow, or unbounded: %+v", query)
		}
	}
}

func TestRootWispSQLiteRetirementScope(t *testing.T) {
	store, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("local"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.(*beads.SQLiteStore).CloseStore() })
	graph, err := storebinding.NewBeadsGraphStore(store)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{beadmeta.KindWisp, beadmeta.KindWorkflow} {
		root := seedRootWisp(t, store, "current", kind)
		next := seedRootWisp(t, store, "next", kind)
		if err := BurnRootWisp(graph, root.ID, "loop", "seat-1", false); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Get(root.ID); !errors.Is(err, beads.ErrNotFound) {
			t.Fatalf("root was not deleted: %v", err)
		}
		got, err := store.Get(next.ID)
		if err != nil || !reflect.DeepEqual(got, next) {
			t.Fatalf("next root changed: %+v, %v", got, err)
		}
		if _, err := store.Create(beads.Bead{Title: "closed child", ParentID: next.ID, Status: "closed"}); err != nil {
			t.Fatal(err)
		}
		if err := BurnRootWisp(graph, next.ID, "loop", "seat-1", false); err == nil {
			t.Fatal("SQLite root with a closed child was burned")
		}
	}
}

func TestRootWispBurnRefusesStructureAddedAfterPreflight(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, change := range []string{"child", "closed member", "incoming dependency", "outgoing dependency"} {
			t.Run(backend+"/"+change, func(t *testing.T) {
				var store beads.Store = beads.NewMemStore()
				if backend == "sqlite" {
					var err error
					store, err = beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("local"))
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = store.(*beads.SQLiteStore).CloseStore() })
				}
				graph, err := storebinding.NewBeadsGraphStore(store)
				if err != nil {
					t.Fatal(err)
				}
				root := seedRootWisp(t, store, "current", beadmeta.KindWorkflow)
				var before []beads.Bead
				var incoming, outgoing []beads.Dep
				query := beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth}
				fault := rootWispFaultGraph{GraphStore: graph, before: func() {
					var err error
					switch change {
					case "child":
						_, err = store.Create(beads.Bead{Title: "concurrent child", ParentID: root.ID})
					case "closed member":
						_, err = store.Create(beads.Bead{
							Title: "concurrent member", Status: "closed",
							Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
						})
					case "incoming dependency":
						err = store.DepAdd("work-1", root.ID, "blocks")
					case "outgoing dependency":
						err = store.DepAdd(root.ID, "work-1", "related")
					}
					if err != nil {
						t.Fatal(err)
					}
					if before, err = store.List(query); err != nil {
						t.Fatal(err)
					}
					if incoming, err = store.DepList(root.ID, "up"); err != nil {
						t.Fatal(err)
					}
					if outgoing, err = store.DepList(root.ID, "down"); err != nil {
						t.Fatal(err)
					}
				}}
				if err := BurnRootWisp(fault, root.ID, "loop", "seat-1", false); err == nil {
					t.Fatal("burn accepted a graph structure added after preflight")
				}
				after, err := store.List(query)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatalf("refused burn changed rows: %v", err)
				}
				for direction, want := range map[string][]beads.Dep{"up": incoming, "down": outgoing} {
					got, err := store.DepList(root.ID, direction)
					if err != nil || !reflect.DeepEqual(got, want) {
						t.Fatalf("refused burn changed %s dependencies: %v", direction, err)
					}
				}
			})
		}
	}
}
