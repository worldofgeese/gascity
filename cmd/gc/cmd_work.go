package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/spf13/cobra"
)

type sharedWorkCommandContext struct {
	cityPath  string
	cfg       *config.City
	work      beads.Store
	sessions  beads.SessionStore
	authority beads.SharedExecutionStore
	close     func() error
}

var sharedWorkCommandProvider = newSessionProvider

func openSharedWorkCommandContext(stderr io.Writer) (*sharedWorkCommandContext, error) {
	cityPath, err := resolveCity()
	if err != nil {
		return nil, err
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		return nil, err
	}
	if cfg.Beads.SharedWork == nil {
		return nil, fmt.Errorf("shared work is not selected: configure beads.shared_work")
	}
	if err := cfg.Beads.SharedWork.Validate(); err != nil {
		return nil, err
	}
	resolveRigPaths(cityPath, cfg.Rigs)
	rigPath := sharedWorkRigRoot(cityPath, cfg)
	if rigPath == "" {
		return nil, fmt.Errorf("shared work rig %q is not bound", cfg.Beads.SharedWork.Rig)
	}
	work, err := oneShotRigStoreOpener(cfg)(rigPath, cityPath)
	if err != nil {
		return nil, err
	}
	authority, err := beads.ResolveSharedExecutionStore(work)
	if err != nil {
		return nil, errors.Join(err, closeBeadStoreHandle(work))
	}
	cityStore, err := openCityStoreAtWithConfig(cityPath, cfg)
	if err != nil {
		return nil, errors.Join(err, closeBeadStoreHandle(work))
	}
	return &sharedWorkCommandContext{
		cityPath: cityPath, cfg: cfg, work: work, authority: authority,
		sessions: beads.SessionStore{Store: cliSessionStore(cityStore, cfg, cityPath)},
		close: func() error {
			return errors.Join(closeBeadStoreHandle(work), closeBeadStoreHandle(cityStore))
		},
	}, nil
}

func sharedExecutionEnvironmentPresent() bool {
	return os.Getenv("GC_SHARED_EXECUTION_ID") != "" || os.Getenv("GC_SHARED_WORK_ID") != "" ||
		os.Getenv("GC_SHARED_WORK_SCOPE") != ""
}

// originalGrant cross-checks the launch environment against the calling
// session's immutable binding. It NEVER takes authority from the work row.
func (c *sharedWorkCommandContext) originalGrant() (beads.ExecutionGrant, error) {
	if c.authority == nil || c.sessions.Store == nil {
		return beads.ExecutionGrant{}, beads.ErrSharedExecutionUnsupported
	}
	grant := beads.ExecutionGrant{BeadID: os.Getenv("GC_SHARED_WORK_ID"), ID: os.Getenv("GC_SHARED_EXECUTION_ID")}
	if err := grant.Validate(); err != nil {
		return beads.ExecutionGrant{}, err
	}
	id := os.Getenv("GC_SESSION_ID")
	if id == "" || id != strings.TrimSpace(id) || c.cfg == nil || c.cfg.Beads.SharedWork == nil ||
		os.Getenv("GC_SHARED_WORK_SCOPE") != c.cfg.Beads.SharedWork.Rig {
		return beads.ExecutionGrant{}, beads.ErrExecutionRequired
	}
	info, err := session.NewStore(c.sessions).Get(id)
	if err != nil {
		return beads.ExecutionGrant{}, fmt.Errorf("reading original shared session: %w", err)
	}
	original, err := info.ExecutionGrant()
	if err != nil || info.Closed || original != grant || info.SharedWorkScope != c.cfg.Beads.SharedWork.Rig {
		return beads.ExecutionGrant{}, beads.ErrExecutionRequired
	}
	return grant, nil
}

func (c *sharedWorkCommandContext) current() (beads.Bead, error) {
	grant, err := c.originalGrant()
	if err != nil {
		return beads.Bead{}, err
	}
	return c.authority.InspectExecution(grant)
}

func (c *sharedWorkCommandContext) mutate(mutation beads.ExecutionMutation) (beads.Bead, error) {
	grant, err := c.originalGrant()
	if err != nil {
		return beads.Bead{}, err
	}
	return c.authority.MutateExecution(grant, mutation)
}

func withSharedWorkCommand(stderr io.Writer, run func(*sharedWorkCommandContext) error) (err error) {
	state, err := openSharedWorkCommandContext(stderr)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, state.close()) }()
	return run(state)
}

func newSharedWorkCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "work",
		Short: "Operate the current immutable shared-work execution",
		Long: `Shared work requires an explicitly selected beads.shared_work configuration
and a store implementing the full execution/lease authority contract. There is
no legacy or unconditional-write fallback. The native file store supports the
cooperative local protocol; the bd/Enterprise adapter is deliberately unsupported.

Worker operations use the ORIGINAL grant injected at session launch. They cannot
target another bead or adopt its current assignee. Human comments and descriptions
are preserved. Lease expiry permits exact-ID reclaim; committed reclaim, not the
clock alone, revokes the old grant.`,
	}
	cmd.AddCommand(&cobra.Command{
		Use: "show", Short: "Show this session's currently authorized work", Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return withSharedWorkCommand(stderr, func(state *sharedWorkCommandContext) error {
				b, err := state.current()
				if err != nil {
					return err
				}
				return writeCLIJSONLine(stdout, b)
			})
		},
	})
	var title, comment string
	var priority int
	var metadata []string
	update := &cobra.Command{
		Use: "update", Short: "Guardedly update title, priority or non-reserved metadata", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			mutation := beads.ExecutionMutation{Operation: beads.ExecutionUpdate, AppendComment: comment}
			if command.Flags().Changed("title") {
				mutation.Update.Title = &title
			}
			if command.Flags().Changed("priority") {
				mutation.Update.Priority = &priority
			}
			if len(metadata) > 0 {
				mutation.Update.Metadata = make(map[string]string)
				for _, pair := range metadata {
					key, value, ok := strings.Cut(pair, "=")
					if !ok || key == "" {
						return fmt.Errorf("metadata must be key=value, got %q", pair)
					}
					mutation.Update.Metadata[key] = value
				}
			}
			if mutation.Update.Title == nil && mutation.Update.Priority == nil && len(metadata) == 0 && comment == "" {
				return fmt.Errorf("provide --title, --priority, --metadata or --comment")
			}
			return runSharedWorkMutation(stderr, stdout, mutation)
		},
	}
	update.Flags().StringVar(&title, "title", "", "New title")
	update.Flags().IntVar(&priority, "priority", 0, "New priority (not an eligibility filter)")
	update.Flags().StringArrayVar(&metadata, "metadata", nil, "Non-reserved key=value (repeatable)")
	update.Flags().StringVar(&comment, "comment", "", "Append a comment, preserving existing human context")
	cmd.AddCommand(update)
	cmd.AddCommand(&cobra.Command{
		Use: "comment <text>", Short: "Append a comment under the current execution grant", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if strings.TrimSpace(args[0]) == "" {
				return fmt.Errorf("comment must not be empty")
			}
			return runSharedWorkMutation(stderr, stdout, beads.ExecutionMutation{Operation: beads.ExecutionUpdate, AppendComment: args[0]})
		},
	})
	for _, operation := range []beads.ExecutionOperation{beads.ExecutionComplete, beads.ExecutionRelease, beads.ExecutionRenew} {
		command := &cobra.Command{
			Use: string(operation), Short: "Guardedly " + string(operation) + " this execution", Args: cobra.NoArgs,
			RunE: func(_ *cobra.Command, _ []string) error {
				return runSharedWorkMutation(stderr, stdout, beads.ExecutionMutation{Operation: operation})
			},
		}
		cmd.AddCommand(command)
	}
	cmd.AddCommand(&cobra.Command{
		Use: "reclaim <exact-id>", Short: "Ask the authority to reclaim exactly one expired execution", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if sharedExecutionEnvironmentPresent() || os.Getenv("GC_SESSION_ID") != "" {
				return fmt.Errorf("gc work reclaim is a controller/operator operation, not an execution-owned mutation")
			}
			return withSharedWorkCommand(stderr, func(state *sharedWorkCommandContext) error {
				b, reclaimed, err := state.authority.ReclaimExecution(args[0])
				if err != nil {
					return err
				}
				return writeCLIJSONLine(stdout, struct {
					Reclaimed bool       `json:"reclaimed"`
					Bead      beads.Bead `json:"bead"`
				}{reclaimed, b})
			})
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use: "run-once", Short: "Run one shared renewal/recovery/acquisition pass (may start workers)", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if sharedExecutionEnvironmentPresent() || os.Getenv("GC_SESSION_ID") != "" {
				return fmt.Errorf("gc work run-once is a controller/operator operation, not an execution-owned mutation")
			}
			return withSharedWorkCommand(stderr, func(state *sharedWorkCommandContext) error {
				provider, err := sharedWorkCommandProvider()
				if err != nil {
					return err
				}
				r, err := newSharedWorkRuntime(state.cityPath, state.cfg, state.work, state.sessions, provider)
				if err != nil {
					return err
				}
				result, err := r.tick(command.Context())
				if err != nil {
					return err
				}
				return writeCLIJSONLine(stdout, result)
			})
		},
	})
	return cmd
}

func runSharedWorkMutation(stderr, stdout io.Writer, mutation beads.ExecutionMutation) error {
	return withSharedWorkCommand(stderr, func(state *sharedWorkCommandContext) error {
		b, err := state.mutate(mutation)
		if err != nil {
			return err
		}
		return writeCLIJSONLine(stdout, b)
	})
}

func sharedHookClaim(opts hookCommandOptions, stdout, stderr io.Writer) int {
	if opts.Claim {
		if marker := hookClaimNonTurnMarker(os.Environ()); marker != "" {
			return writeHookClaimNonTurnDrain(marker, hookClaimOptions{JSON: opts.JSON}, stdout, stderr)
		}
	}
	err := withSharedWorkCommand(stderr, func(state *sharedWorkCommandContext) error {
		b, err := state.current()
		if err != nil {
			return err
		}
		if !opts.Claim {
			return writeCLIJSONLine(stdout, b)
		}
		// This is retrieval of the already awarded original grant, not legacy
		// claim/adoption, work-query execution or continuation preassignment.
		return writeHookClaimResultLine(hookClaimJSONResult{
			SchemaVersion: "1", OK: true, Command: hookClaimCommandName, Action: "work",
			BeadID: b.ID, Assignee: b.Assignee,
		}, opts.JSON, stdout)
	})
	if err != nil {
		fmt.Fprintf(stderr, "gc hook: shared execution refused: %v\n", err) //nolint:errcheck
		if opts.Claim && errors.Is(err, beads.ErrExecutionLost) {
			return writeHookClaimSuspensionDrain("shared_execution_lost", opts, stdout, stderr)
		}
		return 1
	}
	return 0
}

func sharedHookCurrent(idOnly bool, stdout, stderr io.Writer) int {
	err := withSharedWorkCommand(stderr, func(state *sharedWorkCommandContext) error {
		b, err := state.current()
		if err != nil {
			return err
		}
		if idOnly {
			_, err = fmt.Fprintln(stdout, b.ID)
		} else {
			_, err = fmt.Fprintf(stdout, "%s (shared execution %s)\n", b.ID, b.Assignee)
		}
		return err
	})
	if err != nil {
		fmt.Fprintf(stderr, "gc hook current: %v\n", err) //nolint:errcheck
		return 1
	}
	return 0
}
