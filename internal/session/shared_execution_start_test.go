package session

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestSharedExecutionOneShotStartAndOriginalIdentity(t *testing.T) {
	for _, startedCreate := range []bool{false, true} {
		t.Run(map[bool]string{false: "deferred", true: "started"}[startedCreate], func(t *testing.T) {
			work := beads.NewMemStore()
			b, _ := work.Create(beads.Bead{Title: "Human task"})
			grant, won, err := work.AcquireExecution(b.ID, beads.ExecutionOwner{CityID: "city-a"}, time.Minute)
			if err != nil || !won {
				t.Fatalf("acquire: %v %v", won, err)
			}
			ctx := WithSharedExecutionStart(context.Background(), grant, work)
			meta, err := SharedExecutionMetadata(grant, "team")
			if err != nil {
				t.Fatal(err)
			}
			store, provider := beads.NewMemStore(), runtime.NewFake()
			manager := NewManagerWithOptions(store, provider)
			info, err := manager.CreateSession(ctx, CreateOptions{
				Template: "worker", Command: "fixture-agent", Provider: "fixture", WorkDir: t.TempDir(),
				ExtraMeta: meta, BeadOnly: !startedCreate,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !startedCreate {
				if err := manager.StartRuntimeOnly(ctx, info.ID, "fixture-agent", runtime.Config{}); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := info.ExecutionGrant(); err != nil || got != grant {
				t.Fatalf("persisted original identity = %+v, %v", got, err)
			}
			if provider.CountCalls("Start", info.SessionName) != 1 {
				t.Fatalf("want one launch: %+v", provider.Calls)
			}
			for _, call := range provider.Calls {
				if call.Method == "Start" && (call.Config.Env["GC_SHARED_WORK_ID"] != grant.BeadID ||
					call.Config.Env["GC_SHARED_EXECUTION_ID"] != grant.ID || call.Config.Env["BEADS_ACTOR"] != grant.ID) {
					t.Fatalf("runtime did not receive original grant: %+v", call.Config.Env)
				}
			}
			if err := provider.Stop(info.SessionName); err != nil {
				t.Fatal(err)
			}
			for _, start := range []func(context.Context, string, string, runtime.Config) error{manager.Start, manager.StartRuntimeOnly, manager.Attach} {
				if err := start(context.Background(), info.ID, "fixture-agent", runtime.Config{}); !errors.Is(err, beads.ErrExecutionRequired) {
					t.Fatalf("unbound restart = %v", err)
				}
				if err := start(ctx, info.ID, "fixture-agent", runtime.Config{}); !errors.Is(err, beads.ErrExecutionAlreadyStarted) {
					t.Fatalf("replayed launch = %v", err)
				}
			}
			if provider.CountCalls("Start", info.SessionName) != 1 {
				t.Fatal("replayed grant launched a duplicate")
			}
		})
	}
}

func TestSharedExecutionStartRejectsLostGrantAndBorrowedIdentity(t *testing.T) {
	work := beads.NewMemStore()
	b, _ := work.Create(beads.Bead{Title: "Human task"})
	old, _, _ := work.AcquireExecution(b.ID, beads.ExecutionOwner{CityID: "a"}, time.Minute)
	ctx := WithSharedExecutionStart(context.Background(), old, work)
	meta, _ := SharedExecutionMetadata(old, "team")
	store, provider := beads.NewMemStore(), runtime.NewFake()
	manager := NewManagerWithOptions(store, provider)
	info, err := manager.CreateSession(ctx, CreateOptions{
		Template: "worker", Command: "fixture-agent", Provider: "fixture", WorkDir: t.TempDir(), ExtraMeta: meta, BeadOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := work.MutateExecution(old, beads.ExecutionMutation{Operation: beads.ExecutionRelease}); err != nil {
		t.Fatal(err)
	}
	current, _, _ := work.AcquireExecution(b.ID, beads.ExecutionOwner{CityID: "b"}, time.Minute)
	if err := manager.Start(ctx, info.ID, "fixture-agent", runtime.Config{}); !errors.Is(err, beads.ErrExecutionLost) {
		t.Fatalf("revoked original grant start = %v", err)
	}
	borrowed := WithSharedExecutionStart(context.Background(), current, work)
	if err := manager.Start(borrowed, info.ID, "fixture-agent", runtime.Config{}); !errors.Is(err, beads.ErrExecutionRequired) {
		t.Fatalf("borrowed successor identity start = %v", err)
	}
	if provider.CountCalls("Start", info.SessionName) != 0 {
		t.Fatal("stale session was launched")
	}
}

type reclaimDuringExecutionStart struct {
	*runtime.Fake
	work     *beads.FileStore
	clock    *clock.Fake
	id       string
	unrouted []string
}

func (p *reclaimDuringExecutionStart) RouteACP(string) {}
func (p *reclaimDuringExecutionStart) Unroute(name string) {
	p.unrouted = append(p.unrouted, name)
}

func (p *reclaimDuringExecutionStart) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if err := p.Fake.Start(ctx, name, cfg); err != nil {
		return err
	}
	p.clock.Advance(time.Minute)
	_, _, err := p.work.ReclaimExecution(p.id)
	return err
}

func TestSharedExecutionRevokedDuringProviderStartIsStopped(t *testing.T) {
	for _, mode := range []string{"create", "normal", "runtime-only"} {
		t.Run(mode, func(t *testing.T) {
			clk := &clock.Fake{Time: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
			work, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "work.json"), beads.WithFileStoreExecutionClock(clk.Now))
			if err != nil {
				t.Fatal(err)
			}
			b, _ := work.Create(beads.Bead{Title: "Human task"})
			grant, _, _ := work.AcquireExecution(b.ID, beads.ExecutionOwner{CityID: "a"}, time.Minute)
			provider := &reclaimDuringExecutionStart{Fake: runtime.NewFake(), work: work, clock: clk, id: b.ID}
			manager := NewManagerWithOptions(beads.NewMemStore(), provider)
			meta, _ := SharedExecutionMetadata(grant, "team")
			ctx := WithSharedExecutionStart(context.Background(), grant, work)
			info, err := manager.CreateSession(ctx, CreateOptions{
				Template: "worker", ExplicitName: "unique-execution", Command: "fixture-agent", Provider: "fixture",
				WorkDir: t.TempDir(), ExtraMeta: meta, Transport: "acp", BeadOnly: mode != "create",
			})
			if mode != "create" {
				if err != nil {
					t.Fatal(err)
				}
				start := manager.Start
				if mode == "runtime-only" {
					start = manager.StartRuntimeOnly
				}
				err = start(ctx, info.ID, "fixture-agent", runtime.Config{})
			}
			if !errors.Is(err, beads.ErrExecutionLost) || provider.IsRunning("unique-execution") {
				t.Fatalf("revoked runtime accepted: err=%v running=%v", err, provider.IsRunning("unique-execution"))
			}
			if len(provider.unrouted) != 1 || provider.unrouted[0] != "unique-execution" {
				t.Fatalf("revoked startup leaked its ACP route: %v", provider.unrouted)
			}
		})
	}
}
