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

// BurnRootWisp deletes exactly one standalone root-only formula run, without a
// digest, close, cascade, or search of another store. Shape checks include
// closed graph members; the deletion atomically checks graph isolation and the
// root's observed revision. Providers without that capability fail closed.
func BurnRootWisp(store storebinding.GraphStore, id, formula, assignee string, dryRun bool) error {
	if err := validateRootWispSelectors(formula, assignee); err != nil {
		return err
	}
	if id == "" || strings.TrimSpace(id) != id {
		return fmt.Errorf("root wisp: an exact root id is required")
	}
	query := beads.ListQuery{IDs: []string{id}, IncludeClosed: true}
	roots, err := listRootWispRows(store, query)
	if err != nil {
		return err
	}
	if len(roots) != 1 {
		if len(roots) == 0 {
			return fmt.Errorf("root wisp %q: %w in graph store", id, beads.ErrNotFound)
		}
		return fmt.Errorf("root wisp %q: graph store returned multiple exact targets", id)
	}
	root := roots[0]
	if err := validateRootWisp(store, root, formula, assignee); err != nil {
		return err
	}
	if dryRun {
		return nil
	}
	if err := store.DeleteIsolatedIfMatch(id, root.Revision); err != nil {
		return fmt.Errorf("burn root wisp %q: %w", id, err)
	}
	remaining, err := listRootWispRows(store, query)
	if err != nil {
		return fmt.Errorf("verify burn of root wisp %q: %w", id, err)
	}
	if len(remaining) != 0 {
		return fmt.Errorf("burn root wisp %q: target still exists", id)
	}
	return nil
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
	if root.ID == "" || root.Assignee != assignee ||
		root.Ref != formula || root.Metadata[beadmeta.FormulaNameMetadataKey] != formula {
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
