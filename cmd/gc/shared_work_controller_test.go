package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The barrier changes only read timing; the actual claim, persistence and lease
// decisions remain those of two independently opened native FileStore handles.
type sharedReadyBarrier struct {
	beads.Store
	arrived *sync.WaitGroup
	once    sync.Once
}

func (s *sharedReadyBarrier) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	rows, err := s.Store.Ready(query...)
	s.once.Do(func() {
		s.arrived.Done()
		s.arrived.Wait()
	})
	return rows, err
}

func (s *sharedReadyBarrier) SharedExecutionHandle() (beads.SharedExecutionStore, error) {
	return beads.ResolveSharedExecutionStore(s.Store)
}

func sharedRuntimeStartCount(provider *runtime.Fake) int {
	count := 0
	for _, call := range provider.SnapshotCalls() {
		if call.Method == "Start" {
			count++
		}
	}
	return count
}

func sharedRuntimeFixture(t *testing.T, name, rigDir, workPath string, clk *clock.Fake) (*CityRuntime, *beads.FileStore, *beads.FileStore, *runtime.Fake) {
	t.Helper()
	city := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Join(city, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(city, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	work, err := beads.OpenFileStore(fsys.OSFS{}, workPath, beads.WithFileStoreExecutionClock(clk.Now))
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(city, ".gc", "beads.json"))
	if err != nil {
		t.Fatal(err)
	}
	provider := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: name},
		Beads: config.BeadsConfig{
			Provider: "file",
			SharedWork: &config.SharedWorkConfig{
				Rig: "team", Template: "team/worker", Lease: "1m",
			},
		},
		Rigs: []config.Rig{{Name: "team", Path: rigDir}},
		Agents: []config.Agent{{
			Name: "worker", Dir: "team", Provider: "fixture", WorkDir: filepath.Join(city, "work"),
		}},
		Providers: map[string]config.ProviderSpec{"fixture": {Command: "sh"}},
	}
	cr := &CityRuntime{
		cityPath: city, cityName: name, cfg: cfg, sp: provider,
		sessionDrains: newDrainTracker(), rec: events.Discard, stdout: io.Discard, stderr: io.Discard,
		standaloneCityStore: sessions, standaloneRigStores: map[string]beads.Store{
			"team": wrapStoreWithBeadPolicies(work, cfg),
		},
	}
	return cr, work, sessions, provider
}

func TestSharedWorkTwoCitiesLeaseRecoveryAndStaleSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GC_BEADS", "file")
	clk := &clock.Fake{Time: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
	rig := t.TempDir()
	path := filepath.Join(rig, "shared.json")
	a, first, aSessions, aProvider := sharedRuntimeFixture(t, "city-a", rig, path, clk)
	b, second, bSessions, bProvider := sharedRuntimeFixture(t, "city-b", rig, path, clk)
	work, err := first.Create(beads.Bead{
		Title: "Human-created, unlabelled task", Description: "Keep these human notes",
		Metadata: map[string]string{"human": "keep", beads.ExecutionCommentsKey: `[{"text":"human context","author":"Ada"}]`},
	})
	if err != nil {
		t.Fatal(err)
	}
	barrier := &sync.WaitGroup{}
	barrier.Add(2)
	for _, cr := range []*CityRuntime{a, b} {
		cr.standaloneRigStores["team"] = &sharedReadyBarrier{Store: cr.standaloneRigStores["team"], arrived: barrier}
	}
	cities := []*CityRuntime{a, b}
	workStores := []*beads.FileStore{first, second}
	sessionStores := []*beads.FileStore{aSessions, bSessions}
	providers := []*runtime.Fake{aProvider, bProvider}
	var results [2]sharedWorkTickResult
	var errs [2]error
	var racing sync.WaitGroup
	for i := range cities {
		racing.Add(1)
		go func(i int) {
			defer racing.Done()
			results[i], errs[i] = cities[i].sharedWorkTick(context.Background())
		}(i)
	}
	racing.Wait()
	if errs[0] != nil || errs[1] != nil || len(results[0].Launched)+len(results[1].Launched) != 1 {
		t.Fatalf("single winner: results=%+v errors=%v", results, errs)
	}
	winner := 0
	if len(results[1].Launched) == 1 {
		winner = 1
	}
	loser := 1 - winner
	old := results[winner].Launched[0]
	if providers[winner].CountCalls("Start", old.SessionName) != 1 || sharedRuntimeStartCount(providers[loser]) != 0 {
		t.Fatalf("loser launched or winner duplicated: %+v / %+v", providers[0].Calls, providers[1].Calls)
	}
	local, err := session.NewStore(beads.SessionStore{Store: sessionStores[loser]}).ListAll(session.ListAllOptions{})
	if err != nil || len(local) != 0 {
		t.Fatalf("loser created a session: %+v, %v", local, err)
	}
	clk.Advance(59 * time.Second)
	renewal, err := cities[winner].sharedWorkTick(context.Background())
	if err != nil || renewal.Renewed != 1 || len(renewal.Launched) != 0 {
		t.Fatalf("live renewal = %+v, %v", renewal, err)
	}
	clk.Advance(time.Second)
	foreign, err := cities[loser].sharedWorkTick(context.Background())
	if err != nil || len(foreign.Reclaimed) != 0 || len(foreign.Launched) != 0 {
		t.Fatalf("foreign live lease treated as local orphan: %+v %v", foreign, err)
	}
	if err := providers[winner].Stop(old.SessionName); err != nil {
		t.Fatal(err)
	}
	clk.Advance(59 * time.Second)
	takeover, err := cities[loser].sharedWorkTick(context.Background())
	if err != nil || len(takeover.Reclaimed) != 1 || len(takeover.Launched) != 1 {
		t.Fatalf("crash recovery = %+v, %v", takeover, err)
	}
	current := takeover.Launched[0]
	if old.Grant.ID == current.Grant.ID || old.SessionName == current.SessionName {
		t.Fatalf("grant/runtime identity reused: old=%+v current=%+v", old, current)
	}
	before, err := workStores[loser].InspectExecution(current.Grant)
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []beads.ExecutionOperation{beads.ExecutionUpdate, beads.ExecutionComplete, beads.ExecutionRelease, beads.ExecutionRenew} {
		if _, err := workStores[winner].MutateExecution(old.Grant, beads.ExecutionMutation{Operation: operation}); !errors.Is(err, beads.ErrExecutionLost) {
			t.Fatalf("old execution %s = %v", operation, err)
		}
	}
	retirement, err := cities[winner].sharedWorkTick(context.Background())
	if err != nil || len(retirement.Retired) != 1 || len(retirement.Launched) != 0 {
		t.Fatalf("old session retirement = %+v, %v", retirement, err)
	}
	after, err := workStores[loser].InspectExecution(current.Grant)
	if err != nil || !reflect.DeepEqual(before, after) || !providers[loser].IsRunning(current.SessionName) {
		t.Fatalf("old city changed successor: %+v, %v", after, err)
	}
	done, err := workStores[loser].MutateExecution(current.Grant, beads.ExecutionMutation{Operation: beads.ExecutionComplete, AppendComment: "finished"})
	if err != nil || done.Status != "closed" || done.Assignee != "" || done.Description != work.Description || done.Metadata["human"] != "keep" {
		t.Fatalf("completion failed or replaced human content: %+v, %v", done, err)
	}
	finished, err := cities[loser].sharedWorkTick(context.Background())
	if err != nil || len(finished.Retired) != 1 || providers[loser].IsRunning(current.SessionName) {
		t.Fatalf("completed worker not retired: %+v, %v", finished, err)
	}
}

func TestSharedWorkLegacyReconcileAndSweepsDoNotRestartOrRetireGrant(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GC_BEADS", "file")
	rig := t.TempDir()
	clk := &clock.Fake{Time: time.Now().UTC()}
	cr, work, sessions, provider := sharedRuntimeFixture(t, "legacy-passes", rig, filepath.Join(rig, "work.json"), clk)
	if _, err := work.Create(beads.Bead{Title: "Human task"}); err != nil {
		t.Fatal(err)
	}
	first, err := cr.sharedWorkTick(context.Background())
	if err != nil || len(first.Launched) != 1 {
		t.Fatalf("initial shared launch: %+v, %v", first, err)
	}
	launch := first.Launched[0]
	if err := provider.Stop(launch.SessionName); err != nil {
		t.Fatal(err)
	}
	front := session.NewStore(beads.SessionStore{Store: sessions})
	// Simulate a stale pending create visible to older controller phases.
	if err := front.ApplyPatch(launch.SessionID, session.MetadataPatch{
		"state": "creating", "creation_complete_at": "", "pending_create_claim": "",
		"pending_create_started_at": clk.Now().Add(-time.Hour).Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	before, err := sessions.Get(launch.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	beforeWork, err := work.InspectExecution(launch.Grant)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := cr.loadSessionBeadSnapshot()
	if poolSweepWouldDrain(snapshot, nil, cr.cfg) {
		t.Fatal("shared session was classified as an undesired legacy pool")
	}
	if n := sweepUndesiredPoolSessionBeads(cr.cityPath, cr.sessionsBeadStore(), cr.rigBeadStores(), snapshot, nil, cr.cfg, provider, false); n != 0 {
		t.Fatalf("legacy pool sweep retired %d shared sessions", n)
	}
	if n := reapStaleSessionBeads(sessions, provider, nil, nil, clk, io.Discard); n != 0 {
		t.Fatalf("legacy stale-create sweep retired %d shared sessions", n)
	}
	env := newReconcilerTestEnv()
	env.store, env.sp, env.cfg, env.clk = sessions, provider, cr.cfg, clk
	env.addDesired(launch.SessionName, "team/worker", false)
	if n := env.reconcile([]beads.Bead{before}); n != 0 {
		t.Fatalf("legacy reconcile woke %d shared sessions: %s", n, env.stderr.String())
	}
	after, err := sessions.Get(launch.SessionID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("legacy lifecycle changed the shared session: %+v, %v", after, err)
	}
	afterWork, err := work.InspectExecution(launch.Grant)
	if err != nil || !reflect.DeepEqual(beforeWork, afterWork) || sharedRuntimeStartCount(provider) != 1 {
		t.Fatal("legacy lifecycle mutated work or replayed a shared launch")
	}
}

func TestSharedWorkCapacityAndOneShotRefusal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GC_BEADS", "file")
	for _, oneShot := range []bool{false, true} {
		t.Run(map[bool]string{false: "capacity", true: "one-shot"}[oneShot], func(t *testing.T) {
			rig := t.TempDir()
			cr, work, _, provider := sharedRuntimeFixture(t, "limits", rig, filepath.Join(rig, "work.json"), &clock.Fake{Time: time.Now()})
			if oneShot {
				cr.cfg.Agents[0].Lifecycle = config.AgentLifecycleOneShot
			}
			var tasks []beads.Bead
			for _, title := range []string{"Human task one", "Human task two"} {
				task, err := work.Create(beads.Bead{Title: title})
				if err != nil {
					t.Fatal(err)
				}
				tasks = append(tasks, task)
			}
			// Compare persisted state: decoded times lack the monotonic reading
			// that Create left in memory, so in-memory copies are not comparable.
			persisted := func(id string) (beads.Bead, error) {
				store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(rig, "work.json"))
				if err != nil {
					return beads.Bead{}, err
				}
				return store.Get(id)
			}
			for i, task := range tasks {
				saved, err := persisted(task.ID)
				if err != nil {
					t.Fatal(err)
				}
				tasks[i] = saved
			}
			first, err := cr.sharedWorkTick(context.Background())
			if oneShot {
				if err == nil || !strings.Contains(err.Error(), "long-lived") || len(first.Launched) != 0 || sharedRuntimeStartCount(provider) != 0 {
					t.Fatalf("one-shot provider was not refused before launch: %+v, %v", first, err)
				}
				for _, task := range tasks {
					got, err := persisted(task.ID)
					if err != nil || !reflect.DeepEqual(task, got) {
						t.Fatalf("one-shot refusal changed ready work: %+v, %v", got, err)
					}
				}
				return
			}
			if err != nil || len(first.Launched) != 1 {
				t.Fatalf("default local capacity: %+v, %v", first, err)
			}
			again, err := cr.sharedWorkTick(context.Background())
			if err != nil || len(again.Launched) != 0 || sharedRuntimeStartCount(provider) != 1 {
				t.Fatalf("repeated tick exceeded capacity: %+v, %v", again, err)
			}
			cr.cfg.Beads.SharedWork.MaxActive = 2
			expanded, err := cr.sharedWorkTick(context.Background())
			if err != nil || len(expanded.Launched) != 1 || sharedRuntimeStartCount(provider) != 2 {
				t.Fatalf("expanded local capacity: %+v, %v", expanded, err)
			}
		})
	}
}

func TestSharedWorkBeadReconcileTickOwnsLaunchAndRetirement(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GC_BEADS", "file")
	rig := t.TempDir()
	clk := &clock.Fake{Time: time.Now().UTC()}
	cr, work, sessions, provider := sharedRuntimeFixture(t, "controller-tick", rig, filepath.Join(rig, "work.json"), clk)
	cr.cfg.Agents[0].MinActiveSessions = intPtr(1)
	cr.cfg.Agents[0].MaxActiveSessions = intPtr(2)
	task, err := work.Create(beads.Bead{Title: "Human-created controller work"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := cr.loadSessionBeadSnapshot()
	result := buildDesiredStateWithSessionBeads(
		cr.cityName, cr.cityPath, clk.Now(), cr.cfg, provider, sessions, cr.rigBeadStores(), snapshot, nil, io.Discard,
	)
	if len(result.State) != 0 || sharedRuntimeStartCount(provider) != 0 {
		t.Fatal("selected shared template was also allocated through legacy pool demand")
	}
	front := session.NewStore(beads.SessionStore{Store: sessions})
	before, err := front.ListAll(session.ListAllOptions{IncludeClosed: true})
	if err != nil || len(before) != 0 {
		t.Fatalf("legacy desired-state builder allocated a shared session: %+v, %v", before, err)
	}
	cr.beadReconcileTick(context.Background(), result, snapshot, nil, true)
	infos, err := front.ListAll(session.ListAllOptions{})
	if err != nil || len(infos) != 1 || sharedRuntimeStartCount(provider) != 1 {
		t.Fatalf("production boot tick did not launch one shared worker: %+v, %v", infos, err)
	}
	info := infos[0]
	grant, err := info.ExecutionGrant()
	if err != nil || grant.BeadID != task.ID || !provider.IsRunning(info.SessionName) {
		t.Fatalf("production tick started an unbound worker: %+v, %v", info, err)
	}
	cr.beadReconcileTick(context.Background(), result, cr.loadSessionBeadSnapshot(), nil, false)
	if !provider.IsRunning(info.SessionName) || sharedRuntimeStartCount(provider) != 1 {
		t.Fatal("steady-state legacy phases stopped or restarted the shared worker")
	}
	if _, err := work.MutateExecution(grant, beads.ExecutionMutation{Operation: beads.ExecutionComplete}); err != nil {
		t.Fatal(err)
	}
	cr.beadReconcileTick(context.Background(), result, cr.loadSessionBeadSnapshot(), nil, false)
	closed, err := front.Get(info.ID)
	if err != nil || !closed.Closed || provider.IsRunning(info.SessionName) {
		t.Fatalf("production tick failed to retire completed work: %+v, %v", closed, err)
	}
}

func TestSharedWorkLegacyMetadataRepairCannotOverwriteSuccessor(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, repair := range []string{"workdir", "root-stamp"} {
		t.Run(repair, func(t *testing.T) {
			rig := t.TempDir()
			clk := &clock.Fake{Time: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
			cr, work, _, _ := sharedRuntimeFixture(t, "metadata-guard", rig, filepath.Join(rig, "work.json"), clk)
			task, err := work.Create(beads.Bead{
				Title: "Human task", Metadata: map[string]string{beadmeta.LegacyWorkDirMetadataKey: "worktrees/task"},
			})
			if err != nil {
				t.Fatal(err)
			}
			old, won, err := work.AcquireExecution(task.ID, beads.ExecutionOwner{CityID: "old-city"}, time.Minute)
			if err != nil || !won {
				t.Fatalf("old acquisition: %v, %v", won, err)
			}
			stale, err := work.MutateExecution(old, beads.ExecutionMutation{
				Operation: beads.ExecutionStart, SessionID: "old-session", SessionName: "old-worker",
				WorkDir: ".gc/worktrees/team/worker-1",
			})
			if err != nil {
				t.Fatal(err)
			}
			clk.Advance(time.Minute)
			if _, reclaimed, err := work.ReclaimExecution(task.ID); err != nil || !reclaimed {
				t.Fatalf("reclaim: %v, %v", reclaimed, err)
			}
			current, won, err := work.AcquireExecution(task.ID, beads.ExecutionOwner{CityID: "new-city"}, time.Minute)
			if err != nil || !won {
				t.Fatalf("successor acquisition: %v, %v", won, err)
			}
			if _, err := work.MutateExecution(current, beads.ExecutionMutation{
				Operation: beads.ExecutionStart, SessionID: "new-session", SessionName: "new-worker", WorkDir: "worktrees/new",
			}); err != nil {
				t.Fatal(err)
			}
			before, err := work.InspectExecution(current)
			if err != nil {
				t.Fatal(err)
			}
			switch repair {
			case "workdir":
				repairPoolSlotWorkDirClobber(cr.cfg, []beads.Bead{stale}, []beads.Store{work}, io.Discard)
			case "root-stamp":
				step := beads.Bead{ID: "legacy-step", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: task.ID}}
				stampRunRootFromStep(cr.cfg, work, step, "stale-worker", "worktrees/stale", true, make(map[string]struct{}), io.Discard)
			}
			after, err := work.InspectExecution(current)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("legacy %s repair overwrote successor metadata: before=%+v after=%+v err=%v", repair, before, after, err)
			}
		})
	}
}
