package molecule

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// ListRootWisps discovers unfinished, standalone root-only formula runs for
// one execution. Its caller supplies the graph-class store, never a federated
// work view. An unsupported matching root is an error, not an absent patrol.
func ListRootWisps(store storebinding.GraphStore, formula, assignee string) ([]beads.Bead, error) {
	if err := validateRootWispSelectors(formula, assignee); err != nil {
		return nil, err
	}
	roots, err := listRootWispRows(store, beads.ListQuery{
		Assignee: assignee,
		Metadata: map[string]string{beadmeta.FormulaNameMetadataKey: formula},
	})
	if err != nil {
		return nil, err
	}
	for _, root := range roots {
		if err := validateRootWisp(store, root, formula, assignee); err != nil {
			return nil, err
		}
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].ID < roots[j].ID })
	return roots, nil
}

// AssignRootWisp assigns an unfinished standalone root in its graph store.
// An existing assignment to the same execution is idempotent; a different
// execution or a revision change is a refusal, never a reassignment.
func AssignRootWisp(store storebinding.GraphStore, id, formula, assignee string) error {
	if err := validateRootWispSelectors(formula, assignee); err != nil {
		return err
	}
	root, err := rootWispByID(store, id)
	if err != nil {
		return err
	}
	if root.Status != "open" && root.Status != "in_progress" {
		return fmt.Errorf("root wisp %q: only unfinished runs can be assigned", id)
	}
	if root.Assignee != "" && root.Assignee != assignee {
		return fmt.Errorf("root wisp %q: already assigned to %q", id, root.Assignee)
	}
	if err := validateRootWisp(store, root, formula, root.Assignee); err != nil {
		return err
	}
	if root.Assignee == assignee {
		return nil
	}
	if err := store.UpdateIfMatch(id, root.Revision, beads.UpdateOpts{Assignee: &assignee}); err != nil {
		return fmt.Errorf("assign root wisp %q: %w", id, err)
	}
	return nil
}

// BurnRootWisp deletes exactly one standalone root-only formula run, without a
// digest, close, cascade, or search of another store. Shape checks include
// closed graph members; the deletion atomically checks graph isolation and the
// root's observed revision. Providers without that capability fail closed.
func BurnRootWisp(store storebinding.GraphStore, id, formula, assignee string, dryRun bool) error {
	if err := validateRootWispSelectors(formula, assignee); err != nil {
		return err
	}
	root, err := rootWispByID(store, id)
	if err != nil {
		return err
	}
	if err := validateRootWisp(store, root, formula, assignee); err != nil {
		return err
	}
	if dryRun {
		return nil
	}
	if err := store.DeleteIsolatedIfMatch(id, root.Revision); err != nil {
		return fmt.Errorf("burn root wisp %q: %w", id, err)
	}
	remaining, err := listRootWispRows(store, beads.ListQuery{IDs: []string{id}, IncludeClosed: true})
	if err != nil {
		return fmt.Errorf("verify burn of root wisp %q: %w", id, err)
	}
	if len(remaining) != 0 {
		return fmt.Errorf("burn root wisp %q: target still exists", id)
	}
	return nil
}

func rootWispByID(store storebinding.GraphStore, id string) (beads.Bead, error) {
	if id == "" || strings.TrimSpace(id) != id {
		return beads.Bead{}, fmt.Errorf("root wisp: an exact root id is required")
	}
	roots, err := listRootWispRows(store, beads.ListQuery{IDs: []string{id}, IncludeClosed: true})
	if err != nil {
		return beads.Bead{}, err
	}
	if len(roots) == 0 {
		return beads.Bead{}, fmt.Errorf("root wisp %q: %w in graph store", id, beads.ErrNotFound)
	}
	if len(roots) != 1 {
		return beads.Bead{}, fmt.Errorf("root wisp %q: graph store returned multiple exact targets", id)
	}
	return roots[0], nil
}

func validateRootWispSelectors(formula, assignee string) error {
	if formula == "" || strings.TrimSpace(formula) != formula {
		return fmt.Errorf("root wisp: an exact formula name is required")
	}
	if assignee == "" || strings.TrimSpace(assignee) != assignee {
		return fmt.Errorf("root wisp: an exact execution assignee is required")
	}
	return nil
}

func validateRootWisp(store storebinding.GraphStore, root beads.Bead, formula, assignee string) error {
	refuse := func(reason string) error {
		return fmt.Errorf("root wisp %q: %s; only standalone root-only formula runs are supported", root.ID, reason)
	}
	// Batch graph creation carries the root identity in gc.step_id, not Ref.
	stepID := root.Metadata[beadmeta.StepIDMetadataKey]
	if root.ID == "" || root.Assignee != assignee ||
		root.Metadata[beadmeta.FormulaNameMetadataKey] != formula ||
		(root.Ref != "" && root.Ref != formula) ||
		(stepID != "" && stepID != formula) ||
		(root.Ref != formula && stepID != formula) {
		return refuse("formula or execution does not match")
	}
	if root.Type != "task" {
		return refuse("not a root-only task")
	}
	switch root.Metadata[beadmeta.KindMetadataKey] {
	case beadmeta.KindWisp:
	case beadmeta.KindWorkflow:
		if root.Metadata[beadmeta.FormulaContractMetadataKey] != beadmeta.FormulaContractGraphV2 {
			return refuse("not a graph formula root")
		}
	default:
		return refuse("not a wisp or workflow root")
	}
	// Instantiate deliberately omits workflow_expanded for root-only graph
	// runs. Presence means this is not that lifecycle, even if steps are gone.
	if root.Metadata[beadmeta.WorkflowExpandedMetadataKey] != "" ||
		root.Metadata[beadmeta.InstantiatingMetadataKey] != "" {
		return refuse("expanded or unfinished materialization")
	}
	if root.ParentID != "" || len(root.Needs) != 0 || len(root.Dependencies) != 0 {
		return refuse("root has a parent or dependencies")
	}
	for _, key := range []string{
		beadmeta.ParentBeadIDMetadataKey,
		beadmeta.SourceBeadIDMetadataKey,
		beadmeta.InputConvoyIDMetadataKey,
	} {
		if root.Metadata[key] != "" {
			return refuse("root is attached through " + key)
		}
	}
	if parent := root.Metadata[beadmeta.RootBeadIDMetadataKey]; parent != "" && parent != root.ID {
		return refuse("root belongs to another workflow")
	}
	for _, query := range []beads.ListQuery{
		{ParentID: root.ID, IncludeClosed: true},
		{Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID}, IncludeClosed: true},
		{Metadata: map[string]string{beadmeta.ParentBeadIDMetadataKey: root.ID}, IncludeClosed: true},
		{Metadata: map[string]string{beadmeta.SourceBeadIDMetadataKey: root.ID}, IncludeClosed: true},
		{Metadata: map[string]string{beadmeta.InputConvoyIDMetadataKey: root.ID}, IncludeClosed: true},
	} {
		members, err := listRootWispRows(store, query)
		if err != nil {
			return fmt.Errorf("inspect root wisp %q members: %w", root.ID, err)
		}
		for _, member := range members {
			if member.ID != root.ID {
				return refuse("root has graph members")
			}
		}
	}
	for _, direction := range []string{"up", "down"} {
		deps, err := store.DepList(root.ID, direction)
		if err != nil {
			return fmt.Errorf("inspect root wisp %q dependencies: %w", root.ID, err)
		}
		if len(deps) != 0 {
			return refuse("root has graph dependencies")
		}
	}
	return nil
}

func listRootWispRows(store storebinding.GraphStore, query beads.ListQuery) ([]beads.Bead, error) {
	query.Live = true
	query.TierMode = beads.TierBoth
	rows, err := store.List(query)
	if err != nil {
		return nil, fmt.Errorf("list root wisps: %w", err)
	}
	exact := make([]beads.Bead, 0, len(rows))
	for _, row := range rows {
		// Some providers implement indexed selectors as pushdown supersets.
		if query.Matches(row) {
			exact = append(exact, row)
		}
	}
	return exact, nil
}
