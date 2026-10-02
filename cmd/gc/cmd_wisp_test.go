package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
)

func runRootWispCommand(args ...string) (string, string, error) {
	var stdout, stderr bytes.Buffer
	cmd := newWispCmd(&stdout, &stderr)
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs(args)
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}

func parseRootWispList(t *testing.T, out string) []beads.Bead {
	t.Helper()
	validateJSONAgainstResultSchema(t, []string{"wisp", "list"}, []byte(out))
	var result wispListJSONResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	return result.Roots
}

func TestWispClassLocalCookRestartAndBurn(t *testing.T) {
	for _, split := range []bool{false, true} {
		for _, formula := range []string{"vapor-work", "graph-vapor"} {
			t.Run(formula+map[bool]string{false: "/single", true: "/split"}[split], func(t *testing.T) {
				cityDir := oneShotCookCity(t)
				if err := os.WriteFile(filepath.Join(cityDir, "formulas", "graph-vapor.formula.toml"), []byte(`
formula = "graph-vapor"
version = 2
contract = "graph.v2"
phase = "vapor"

[[steps]]
id = "first"
title = "First"

[[steps]]
id = "second"
title = "Second"
needs = ["first"]
`), 0o644); err != nil {
					t.Fatal(err)
				}
				var graph beads.Store
				if split {
					graph = splittest.NewClassStore(t, config.BeadClassGraph)
					seedCLIStorageRoutes(t, cityDir, messagingSplitRoutes(graph))
				} else {
					seedCLIStorageRoutes(t, cityDir, nil)
				}
				work, err := openStoreAtForCity(cityDir, cityDir)
				if err != nil {
					t.Fatal(err)
				}
				if graph == nil {
					graph = work
				}
				shared, err := work.Create(beads.Bead{Title: "ordinary shared work", Type: "task", Assignee: "seat-1"})
				if err != nil {
					t.Fatal(err)
				}
				cooked := cookFormula(t, formula)
				root, err := graph.Get(cooked.RootID)
				if err != nil {
					t.Fatal(err)
				}
				if err := graph.Update(root.ID, beads.UpdateOpts{Assignee: &shared.Assignee}); err != nil {
					t.Fatal(err)
				}
				for range 2 {
					out, stderr, err := runRootWispCommand("list", "--formula", formula, "--assignee", "seat-1", "--json")
					if err != nil {
						t.Fatalf("restart lookup: %v\n%s", err, stderr)
					}
					found := parseRootWispList(t, out)
					if len(found) != 1 || found[0].ID != root.ID {
						t.Fatalf("restart lookup = %s, %v; want only %s", out, err, root.ID)
					}
				}
				preview, stderr, err := runRootWispCommand("burn", root.ID, "--formula", formula, "--assignee", "seat-1", "--json")
				if err != nil || !strings.Contains(preview, `"deleted":false`) {
					t.Fatalf("burn preview = %s, %v\n%s", preview, err, stderr)
				}
				if _, err := graph.Get(root.ID); err != nil {
					t.Fatalf("preview deleted root: %v", err)
				}
				out, stderr, err := runRootWispCommand("burn", root.ID, "--formula", formula, "--assignee", "seat-1", "--force", "--json")
				if err != nil || !strings.Contains(out, `"deleted":true`) {
					t.Fatalf("burn = %s, %v\n%s", out, err, stderr)
				}
				validateJSONAgainstResultSchema(t, []string{"wisp", "burn"}, []byte(out))
				if _, err := graph.Get(root.ID); !errors.Is(err, beads.ErrNotFound) {
					t.Fatalf("burn did not delete root: %v", err)
				}
				got, err := work.Get(shared.ID)
				gotJSON, gotJSONErr := json.Marshal(got)
				wantJSON, wantJSONErr := json.Marshal(shared)
				if err != nil || gotJSONErr != nil || wantJSONErr != nil || !bytes.Equal(gotJSON, wantJSON) {
					t.Fatalf("shared work changed: %+v, %v; want %+v", got, err, shared)
				}
				out, stderr, err = runRootWispCommand("list", "--formula", formula, "--assignee", "seat-1", "--json")
				if err != nil {
					t.Fatalf("empty lookup = %s, %v\n%s", out, err, stderr)
				}
				if found := parseRootWispList(t, out); len(found) != 0 {
					t.Fatalf("burned root was rediscovered: %v", found)
				}
				all, err := work.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
				if err != nil || len(all) != 1 || all[0].ID != shared.ID {
					t.Fatalf("ordinary work projection = %v, %v", all, err)
				}
			})
		}
	}
}

func TestWispNeverSearchesForeignWorkStore(t *testing.T) {
	cityDir := oneShotCookCity(t)
	graph := splittest.NewClassStore(t, config.BeadClassGraph)
	seedCLIStorageRoutes(t, cityDir, messagingSplitRoutes(graph))
	work, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := work.Create(beads.Bead{
		Title: "not in this graph store", Type: "task", Ref: "vapor-work", Assignee: "seat-1",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:        beadmeta.KindWisp,
			beadmeta.FormulaNameMetadataKey: "vapor-work",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, stderr, err := runRootWispCommand("list", "--formula", "vapor-work", "--assignee", "seat-1", "--json")
	if err != nil {
		t.Fatalf("lookup searched work store: %s, %v\n%s", out, err, stderr)
	}
	if found := parseRootWispList(t, out); len(found) != 0 {
		t.Fatalf("lookup found foreign work-store root: %v", found)
	}
	out, stderr, err = runRootWispCommand("burn", foreign.ID, "--formula", "vapor-work", "--assignee", "seat-1", "--force")
	if err == nil || out != "" {
		t.Fatalf("foreign-store burn did not refuse: %q, %v\n%s", out, err, stderr)
	}
	if got, err := work.Get(foreign.ID); err != nil || !reflect.DeepEqual(got, foreign) {
		t.Fatalf("foreign root changed: %+v, %v", got, err)
	}
}

func TestWispStorageRefusalIsNotEmptyLookup(t *testing.T) {
	cityDir := oneShotCookCity(t)
	refusal := errors.New("graph binding unavailable")
	seedCLIStorageRoutes(t, cityDir, refusingStorageRoutes("local", refusal))
	for _, args := range [][]string{
		{"list", "--formula", "vapor-work", "--assignee", "seat-1", "--json"},
		{"burn", "gcg-1", "--formula", "vapor-work", "--assignee", "seat-1", "--force"},
	} {
		out, stderr, err := runRootWispCommand(args...)
		if err == nil || out != "" || !strings.Contains(stderr+err.Error(), refusal.Error()) {
			t.Fatalf("storage refusal lost: stdout=%q stderr=%q err=%v", out, stderr, err)
		}
	}
}

func TestWispRequiresExplicitSelectors(t *testing.T) {
	for _, args := range [][]string{
		{"list"},
		{"list", "--formula", "loop"},
		{"list", "--assignee", "seat-1"},
		{"burn", "gcg-1", "--force"},
		{"burn", "gcg-1", "--formula", "loop", "--force"},
		{"burn", "gcg-1", "--assignee", "seat-1", "--force"},
	} {
		out, _, err := runRootWispCommand(args...)
		if err == nil || out != "" {
			t.Fatalf("selectors not required for %q: %q, %v", args, out, err)
		}
	}
}
