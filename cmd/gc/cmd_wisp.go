package main

import (
	"fmt"
	"io"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/storebinding"
	"github.com/spf13/cobra"
)

type wispListJSONResult struct {
	SchemaVersion string       `json:"schema_version"`
	OK            bool         `json:"ok"`
	Roots         []beads.Bead `json:"roots"`
}

type wispBurnJSONResult struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	ID            string `json:"id"`
	Deleted       bool   `json:"deleted"`
	DryRun        bool   `json:"dry_run"`
}

type wispAssignJSONResult struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	ID            string `json:"id"`
	Assignee      string `json:"assignee"`
}

func newWispListCmd(stdout, stderr io.Writer) *cobra.Command {
	var formula, assignee string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Find unfinished root-only formula runs in the graph-class store",
		Long: `Find standalone root-only formula runs for one execution.

Both --formula and --assignee are exact selectors. Closed roots are omitted.
Reads the same graph-class destination as standalone gc formula cook, including
vapor graph.v2 runs. Does not search other stores or change gc bd list.
An unsupported matching graph or unavailable store is an error, not an empty list.
With --json, prints a versioned result containing a roots array.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			store, err := openRootWispGraphStore(stderr)
			if err != nil {
				return formulaCommandError(stderr, "gc wisp list", jsonOutput, err)
			}
			roots, err := molecule.ListRootWisps(store, formula, assignee)
			if err != nil {
				return formulaCommandError(stderr, "gc wisp list", jsonOutput, err)
			}
			if jsonOutput {
				return writeCLIJSONLineOrErr(stdout, stderr, "gc wisp list", wispListJSONResult{
					SchemaVersion: "1", OK: true, Roots: roots,
				})
			}
			for _, root := range roots {
				if _, err := fmt.Fprintf(stdout, "%s\t%s\t%s\n", root.ID, root.Status, root.Assignee); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&formula, "formula", "", "Exact formula name (required)")
	cmd.Flags().StringVar(&assignee, "assignee", "", "Exact execution assignee (required)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output a JSON result with a roots array")
	_ = cmd.MarkFlagRequired("formula")
	_ = cmd.MarkFlagRequired("assignee")
	return cmd
}

func newWispAssignCmd(stdout, stderr io.Writer) *cobra.Command {
	var formula, assignee string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "assign <root-id>",
		Short: "Assign one unfinished root-only run in the graph-class store",
		Long: `Assign an exact standalone root-only run after gc formula cook.

Requires matching --formula and an execution --assignee. An existing assignment
to that execution succeeds without a write; another assignee refuses.
Closed roots, expanded graphs, attachments, graph members, and dependencies
refuse. Assignment checks the observed revision; unsupported providers refuse.
Never searches another store or changes ordinary work assignment.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			store, err := openRootWispGraphStore(stderr)
			if err != nil {
				return formulaCommandError(stderr, "gc wisp assign", jsonOutput, err)
			}
			if err := molecule.AssignRootWisp(store, args[0], formula, assignee); err != nil {
				return formulaCommandError(stderr, "gc wisp assign", jsonOutput, err)
			}
			if jsonOutput {
				return writeCLIJSONLineOrErr(stdout, stderr, "gc wisp assign", wispAssignJSONResult{
					SchemaVersion: "1", OK: true, ID: args[0], Assignee: assignee,
				})
			}
			_, err = fmt.Fprintf(stdout, "Assigned root wisp %s to %s\n", args[0], assignee)
			return err
		},
	}
	cmd.Flags().StringVar(&formula, "formula", "", "Exact formula name (required)")
	cmd.Flags().StringVar(&assignee, "assignee", "", "Exact execution assignee (required)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output the result as JSON")
	_ = cmd.MarkFlagRequired("formula")
	_ = cmd.MarkFlagRequired("assignee")
	return cmd
}

func newWispBurnCmd(stdout, stderr io.Writer) *cobra.Command {
	var formula, assignee string
	var force, dryRun, jsonOutput bool
	cmd := &cobra.Command{
		Use:   "burn <root-id>",
		Short: "Delete exactly one standalone root-only formula run, without a digest",
		Long: `Burn one exact root in the graph-class store.

Requires matching --formula and --assignee. Only standalone root-only runs are
supported: expanded graphs, attachments, graph members, and dependencies refuse.
Never closes the root, cascades to other beads, or sweeps another store.
The delete atomically checks isolation and the root revision; a provider without
atomic isolated deletion refuses. SQLite and file stores support this capability.
Without --force (or with --dry-run), validates and previews without deleting.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			store, err := openRootWispGraphStore(stderr)
			if err != nil {
				return formulaCommandError(stderr, "gc wisp burn", jsonOutput, err)
			}
			preview := !force || dryRun
			if err := molecule.BurnRootWisp(store, args[0], formula, assignee, preview); err != nil {
				return formulaCommandError(stderr, "gc wisp burn", jsonOutput, err)
			}
			if jsonOutput {
				return writeCLIJSONLineOrErr(stdout, stderr, "gc wisp burn", wispBurnJSONResult{
					SchemaVersion: "1", OK: true, ID: args[0], Deleted: !preview, DryRun: preview,
				})
			}
			action := "Burned"
			if preview {
				action = "Would burn"
			}
			_, err = fmt.Fprintf(stdout, "%s root wisp %s (no digest)\n", action, args[0])
			return err
		},
	}
	cmd.Flags().StringVar(&formula, "formula", "", "Exact formula name (required)")
	cmd.Flags().StringVar(&assignee, "assignee", "", "Exact execution assignee (required)")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Delete the validated root")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Validate without deleting")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output the result as JSON")
	_ = cmd.MarkFlagRequired("formula")
	_ = cmd.MarkFlagRequired("assignee")
	cmd.MarkFlagsMutuallyExclusive("force", "dry-run")
	return cmd
}

func openRootWispGraphStore(stderr io.Writer) (storebinding.GraphStore, error) {
	cityPath, err := resolveCity()
	if err != nil {
		return nil, err
	}
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, stderr)
	if err != nil {
		return nil, err
	}
	scope, err := resolveFormulaScope(cfg, cityPath, stderr)
	if err != nil {
		return nil, err
	}
	routes := cliStorageRoutes(cityPath)
	graph, relocated := graphClassBinding(routes)
	if !relocated {
		// A relocated class never needs to open the work store.
		work, err := openStoreAtForCity(scope.storeRoot, cityPath)
		if err != nil {
			return nil, err
		}
		graph = resolveGraphStore(routes, work, cfg, cityPath, nil)
	}
	return storebinding.NewBeadsGraphStore(graph)
}
