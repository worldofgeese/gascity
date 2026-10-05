package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
	"github.com/gastownhall/gascity/internal/worker"
)

type sharedWorkLaunch struct {
	Grant       beads.ExecutionGrant `json:"grant"`
	SessionID   string               `json:"session_id"`
	SessionName string               `json:"session_name"`
}

type sharedWorkTickResult struct {
	Launched  []sharedWorkLaunch `json:"launched"`
	Renewed   int                `json:"renewed"`
	Reclaimed []string           `json:"reclaimed"`
	Retired   []string           `json:"retired"`
}

const sharedWorkStartupInstruction = "You hold one immutable shared-work execution. Run `gc work show` to read the task. Use `gc work comment` to append progress and `gc work complete` to finish, or `gc work release` to return it. Do not use raw bd, gc bd, gc sling, or claim other work. If the original grant is lost, stop; never borrow a successor's identity or restart this grant."

// sharedWorkRuntime uses one shared work authority and this city's session
// ledger. It never treats an absent local session as proof of remote death.
type sharedWorkRuntime struct {
	cityPath  string
	cfg       *config.City
	spec      config.SharedWorkConfig
	agent     *config.Agent
	work      beads.Store
	sessions  beads.SessionStore
	provider  runtime.Provider
	authority beads.SharedExecutionStore
	lease     time.Duration
}

func sharedWorkRigRoot(cityPath string, cfg *config.City) string {
	if cfg == nil || cfg.Beads.SharedWork == nil {
		return ""
	}
	for _, rig := range cfg.Rigs {
		if rig.Name == cfg.Beads.SharedWork.Rig && rig.Path != "" {
			if filepath.IsAbs(rig.Path) {
				return rig.Path
			}
			return filepath.Join(cityPath, rig.Path)
		}
	}
	return ""
}

func newSharedWorkRuntime(cityPath string, cfg *config.City, work beads.Store, sessions beads.SessionStore, provider runtime.Provider) (*sharedWorkRuntime, error) {
	if cfg == nil || cfg.Beads.SharedWork == nil {
		return nil, fmt.Errorf("shared work is not selected: configure beads.shared_work")
	}
	spec := *cfg.Beads.SharedWork
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	authority, err := beads.ResolveSharedExecutionStore(work)
	if err != nil {
		return nil, err
	}
	if sessions.Store == nil || provider == nil {
		return nil, fmt.Errorf("shared work requires a local session store and runtime provider")
	}
	a := config.FindAgent(cfg, spec.Template)
	if a == nil || a.Dir != spec.Rig {
		return nil, fmt.Errorf("shared work template %q must exist in rig %q", spec.Template, spec.Rig)
	}
	for _, named := range cfg.NamedSessions {
		if named.TemplateQualifiedName() == a.QualifiedName() {
			return nil, fmt.Errorf("shared work template %q cannot also back a named session", spec.Template)
		}
	}
	lease, err := spec.LeaseDuration()
	if err != nil {
		return nil, err
	}
	return &sharedWorkRuntime{
		cityPath: cityPath, cfg: cfg, spec: spec, agent: a, work: work,
		sessions: sessions, provider: provider, authority: authority, lease: lease,
	}, nil
}

func (r *sharedWorkRuntime) tick(ctx context.Context) (sharedWorkTickResult, error) {
	if err := ctx.Err(); err != nil {
		return sharedWorkTickResult{}, err
	}
	var result sharedWorkTickResult
	err := session.WithCitySessionIdentifierLocks(r.cityPath, []string{"shared-work/" + r.spec.Rig}, func() error {
		var err error
		result, err = r.tickLocked(ctx)
		return err
	})
	return result, err
}

func (r *sharedWorkRuntime) tickLocked(ctx context.Context) (sharedWorkTickResult, error) {
	var result sharedWorkTickResult
	if err := ctx.Err(); err != nil {
		return result, err
	}
	front := session.NewStore(r.sessions)
	infos, err := front.ListAll(session.ListAllOptions{Live: true})
	if err != nil {
		return result, fmt.Errorf("listing shared sessions: %w", err)
	}
	factory, err := workerFactoryWithConfig(r.cityPath, r.sessions.Store, r.provider, r.cfg)
	if err != nil {
		return result, err
	}
	active := make(map[string]bool)
	for _, info := range infos {
		if !info.IsSharedExecution() {
			continue
		}
		if info.SharedWorkScope != r.spec.Rig {
			return result, fmt.Errorf("shared session %s has missing or different original work scope: %w", info.ID, beads.ErrExecutionRequired)
		}
		grant, err := info.ExecutionGrant()
		if err != nil {
			return result, fmt.Errorf("shared session %s: %w", info.ID, err)
		}
		handle, err := factory.SessionByID(info.ID)
		if err != nil {
			return result, fmt.Errorf("opening shared worker %s: %w", info.ID, err)
		}
		if _, err := r.authority.InspectExecution(grant); err != nil {
			if !errors.Is(err, beads.ErrExecutionLost) && !errors.Is(err, beads.ErrNotFound) {
				return result, err
			}
			// Close only the old execution's uniquely named LOCAL session.
			// Never clear the work row or adopt its successor's identity.
			if err := handle.Close(ctx); err != nil {
				return result, fmt.Errorf("retiring revoked shared worker %s: %w", info.ID, err)
			}
			result.Retired = append(result.Retired, info.ID)
			continue
		}
		active[grant.BeadID] = true
		observed, err := handle.LiveObservation(ctx)
		if err != nil {
			return result, fmt.Errorf("observing shared worker %s: %w", info.ID, err)
		}
		if observed.Alive && !observed.Suspended {
			if _, err := r.authority.MutateExecution(grant, beads.ExecutionMutation{Operation: beads.ExecutionRenew}); err != nil {
				return result, fmt.Errorf("renewing shared worker %s: %w", info.ID, err)
			}
			result.Renewed++
		}
	}
	// Each exact-ID operation evaluates the lease under the authority's lock,
	// serialized against renewal. This snapshot never supplies a lease verdict.
	reader := beads.HandlesFor(r.work).Live
	claimed, err := reader.List(beads.ListQuery{Status: "in_progress", TierMode: beads.TierBoth})
	if err != nil {
		return result, fmt.Errorf("listing shared leases: %w", err)
	}
	for _, b := range claimed {
		if !beads.IsExecutionOwned(b) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		_, reclaimed, err := r.authority.ReclaimExecution(b.ID)
		if err != nil {
			return result, fmt.Errorf("reclaiming shared work %s: %w", b.ID, err)
		}
		if reclaimed {
			delete(active, b.ID)
			result.Reclaimed = append(result.Reclaimed, b.ID)
		}
	}
	state, err := loadSuspensionState(fsys.OSFS{}, r.cityPath)
	if err != nil {
		return result, err
	}
	if isAgentEffectivelySuspendedWith(r.cfg, r.cityPath, r.agent, state) {
		return result, nil
	}
	ready, err := reader.Ready(beads.ReadyQuery{TierMode: beads.TierBoth})
	if err != nil {
		return result, fmt.Errorf("listing shared ready work: %w", err)
	}
	for _, b := range ready {
		if len(active) >= r.spec.ActiveLimit() {
			break
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		launch, won, err := r.acquireAndStart(ctx, b.ID)
		if err != nil {
			return result, err
		}
		if won {
			active[b.ID] = true
			result.Launched = append(result.Launched, launch)
		}
	}
	return result, nil
}

func (r *sharedWorkRuntime) acquireAndStart(ctx context.Context, id string) (sharedWorkLaunch, bool, error) {
	resolved, err := config.ResolveProvider(r.agent, &r.cfg.Workspace, r.cfg.Providers, exec.LookPath)
	if err != nil {
		return sharedWorkLaunch{}, false, err
	}
	if resolved.Lifecycle == config.AgentLifecycleOneShot {
		return sharedWorkLaunch{}, false, fmt.Errorf("shared work requires a long-lived worker; one-shot startup is unsupported")
	}
	transport := config.ResolveSessionCreateTransport(r.agent.Session, resolved)
	if err := validateResolvedSessionTransport(resolved, transport, r.provider); err != nil {
		return sharedWorkLaunch{}, false, err
	}
	command, err := resolvedSessionCommand(r.cityPath, resolved, nil, transport)
	if err != nil {
		return sharedWorkLaunch{}, false, err
	}
	grant, won, err := r.authority.AcquireExecution(id, beads.ExecutionOwner{
		CityID: filepath.Clean(r.cityPath), DisplayName: r.agent.QualifiedName(),
	}, r.lease)
	if err != nil || !won {
		return sharedWorkLaunch{}, false, err
	}
	if err := grant.Validate(); err != nil || grant.BeadID != id {
		return sharedWorkLaunch{}, false, fmt.Errorf("acquisition returned an invalid execution grant: %w", beads.ErrExecutionRequired)
	}
	metadata, err := session.SharedExecutionMetadata(grant, r.spec.Rig)
	if err != nil {
		return sharedWorkLaunch{}, false, err
	}
	// Explicit runtime names have a 64-character limit. This is 224 bits of
	// the random grant; the FULL 256-bit grant remains the only authority.
	name := "shared-" + grant.ID[len("exec-"):len("exec-")+56]
	qualified := workdirutil.SessionQualifiedName(r.cityPath, *r.agent, r.cfg.Rigs, "", name)
	workDir, err := resolveWorkDirForQualifiedName(r.cityPath, r.cfg, r.agent, qualified)
	if err != nil {
		return sharedWorkLaunch{}, false, err
	}
	metadata["agent_name"] = qualified
	if family := resolvedProviderFamilyMetadata(resolved); family != "" {
		metadata["provider_kind"] = family
	}
	handle, err := newWorkerSessionHandleForResolvedRuntimeWithConfig(
		r.cityPath, r.sessions.Store, r.provider, r.cfg, "", name,
		r.agent.QualifiedName(), "Shared work "+id, command, r.agent.Provider, workDir, transport, resolved, metadata,
	)
	if err != nil {
		return sharedWorkLaunch{}, false, err
	}
	info, err := handle.Create(session.WithSharedExecutionStart(ctx, grant, r.authority), worker.CreateModeStarted)
	if err != nil {
		// The provider or session store may have committed before its response
		// failed. No retry/adoption/release here: authoritative expiry recovers.
		return sharedWorkLaunch{}, false, fmt.Errorf("starting shared execution for %s (lease retained for recovery): %w", id, err)
	}
	return sharedWorkLaunch{Grant: grant, SessionID: info.ID, SessionName: info.SessionName}, true, nil
}

func (cr *CityRuntime) sharedWorkTick(ctx context.Context) (sharedWorkTickResult, error) {
	if cr.cfg == nil || cr.cfg.Beads.SharedWork == nil {
		return sharedWorkTickResult{}, nil
	}
	r, err := newSharedWorkRuntime(
		cr.cityPath, cr.cfg, cr.rigBeadStores()[cr.cfg.Beads.SharedWork.Rig], cr.sessionsBeadStore(), cr.sp,
	)
	if err != nil {
		return sharedWorkTickResult{}, err
	}
	return r.tick(ctx)
}
