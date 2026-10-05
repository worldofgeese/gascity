package main

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/session"
)

var errSharedInjected = errors.New("injected execution backend failure")

type failingSharedExecutionStore struct {
	beads.Store
	authority beads.SharedExecutionStore
	failure   string
}

func (s *failingSharedExecutionStore) SharedExecutionHandle() (beads.SharedExecutionStore, error) {
	return s, nil
}

func (s *failingSharedExecutionStore) AcquireExecution(id string, owner beads.ExecutionOwner, ttl time.Duration) (beads.ExecutionGrant, bool, error) {
	if s.failure == "before-acquire" {
		return beads.ExecutionGrant{}, false, errSharedInjected
	}
	grant, won, err := s.authority.AcquireExecution(id, owner, ttl)
	if err == nil && won {
		switch s.failure {
		case "after-acquire":
			return grant, true, errSharedInjected
		case "invalid-grant":
			grant.ID = ""
		}
	}
	return grant, won, err
}

func (s *failingSharedExecutionStore) InspectExecution(grant beads.ExecutionGrant) (beads.Bead, error) {
	if s.failure == "inspect" {
		return beads.Bead{}, errSharedInjected
	}
	return s.authority.InspectExecution(grant)
}

func (s *failingSharedExecutionStore) MutateExecution(grant beads.ExecutionGrant, mutation beads.ExecutionMutation) (beads.Bead, error) {
	if mutation.Operation == beads.ExecutionRenew && s.failure == "renew" {
		return beads.Bead{}, errSharedInjected
	}
	b, err := s.authority.MutateExecution(grant, mutation)
	if err == nil && mutation.Operation == beads.ExecutionStart && s.failure == "after-admission" {
		return beads.Bead{}, errSharedInjected
	}
	return b, err
}

func (s *failingSharedExecutionStore) ReclaimExecution(id string) (beads.Bead, bool, error) {
	if s.failure == "reclaim" {
		return beads.Bead{}, false, errSharedInjected
	}
	return s.authority.ReclaimExecution(id)
}

type sharedCapabilityHidden struct{ beads.Store }

func TestSharedWorkUnsupportedAuthorityNeverStartsOrWrites(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rig := t.TempDir()
	cr, work, sessions, provider := sharedRuntimeFixture(t, "unsupported", rig, filepath.Join(rig, "work.json"), &clock.Fake{Time: time.Now()})
	b, _ := work.Create(beads.Bead{Title: "Human task"})
	before, _ := work.Get(b.ID)
	cr.standaloneRigStores["team"] = &sharedCapabilityHidden{work}
	if _, err := cr.sharedWorkTick(context.Background()); !errors.Is(err, beads.ErrSharedExecutionUnsupported) {
		t.Fatalf("capability refusal = %v", err)
	}
	after, _ := work.Get(b.ID)
	rows, err := session.NewStore(beads.SessionStore{Store: sessions}).ListAll(session.ListAllOptions{IncludeClosed: true})
	if err != nil || !reflect.DeepEqual(before, after) || len(rows) != 0 || sharedRuntimeStartCount(provider) != 0 {
		t.Fatal("unsupported authority fell back to legacy writes or launch")
	}
}

func TestSharedWorkBdCapabilityRefusesBeforeAnyCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rig := t.TempDir()
	cr, _, sessions, provider := sharedRuntimeFixture(t, "bd-refused", rig, filepath.Join(rig, "work.json"), &clock.Fake{Time: time.Now()})
	calls := 0
	unsupported := beads.NewBdStore(rig, func(_, _ string, _ ...string) ([]byte, error) {
		calls++
		return nil, errSharedInjected
	})
	cr.standaloneRigStores["team"] = wrapStoreWithBeadPolicies(unsupported, cr.cfg)
	if _, err := cr.sharedWorkTick(context.Background()); !errors.Is(err, beads.ErrSharedExecutionUnsupported) {
		t.Fatalf("bd adapter capability = %v", err)
	}
	rows, err := session.NewStore(beads.SessionStore{Store: sessions}).ListAll(session.ListAllOptions{IncludeClosed: true})
	if err != nil || calls != 0 || len(rows) != 0 || sharedRuntimeStartCount(provider) != 0 {
		t.Fatalf("unsupported bd adapter invoked a CLI or launch: calls=%d sessions=%d err=%v", calls, len(rows), err)
	}
}

func TestSharedWorkAmbiguousAcquireAdmissionRetryAndRestart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GC_BEADS", "file")
	for _, failure := range []string{"before-acquire", "after-acquire", "invalid-grant", "inspect", "after-admission"} {
		t.Run(failure, func(t *testing.T) {
			rig := t.TempDir()
			clk := &clock.Fake{Time: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
			cr, work, sessions, provider := sharedRuntimeFixture(t, "uncertain", rig, filepath.Join(rig, "work.json"), clk)
			b, _ := work.Create(beads.Bead{Title: "Human task", Metadata: map[string]string{"human": "keep"}})
			before, _ := work.Get(b.ID)
			faults := &failingSharedExecutionStore{Store: work, authority: work, failure: failure}
			cr.standaloneRigStores["team"] = faults
			first, err := cr.sharedWorkTick(context.Background())
			if err == nil || len(first.Launched) != 0 || sharedRuntimeStartCount(provider) != 0 {
				t.Fatalf("uncertain result was launch permission: %+v, %v, %+v", first, err, provider.Calls)
			}
			uncertain, _ := work.Get(b.ID)
			if failure == "before-acquire" && !reflect.DeepEqual(before, uncertain) {
				t.Fatal("failed acquisition performed an unconditional fallback")
			}
			faults.failure = ""
			// Reconstruct the controller, retaining only durable stores and the
			// provider boundary. No in-memory grant is available for adoption.
			restarted := &CityRuntime{
				cityPath: cr.cityPath, cityName: cr.cityName, cfg: cr.cfg, sp: provider,
				standaloneCityStore: sessions, standaloneRigStores: cr.standaloneRigStores,
			}
			retry, err := restarted.sharedWorkTick(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if failure == "before-acquire" {
				if len(retry.Launched) != 1 {
					t.Fatal("known uncommitted failure could not acquire a fresh grant")
				}
				return
			}
			if len(retry.Launched) != 0 || sharedRuntimeStartCount(provider) != 0 {
				t.Fatal("restart reused an ambiguous old grant")
			}
			clk.Advance(time.Minute)
			recovered, err := restarted.sharedWorkTick(context.Background())
			if err != nil || len(recovered.Launched) != 1 || len(recovered.Reclaimed) != 1 {
				t.Fatalf("authoritative recovery failed: %+v, %v", recovered, err)
			}
			current := recovered.Launched[0]
			if current.Grant.ID == uncertain.Assignee || provider.CountCalls("Start", current.SessionName) != 1 {
				t.Fatal("recovery reused old grant or duplicated launch")
			}
			repeat, err := restarted.sharedWorkTick(context.Background())
			if err != nil || len(repeat.Launched) != 0 {
				t.Fatalf("repeat launched another worker: %+v, %v", repeat, err)
			}
		})
	}
}

func TestSharedWorkRenewalFailureAndMissingSessionIdentityRefuse(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GC_BEADS", "file")
	rig := t.TempDir()
	cr, work, sessions, provider := sharedRuntimeFixture(t, "renewal", rig, filepath.Join(rig, "work.json"), &clock.Fake{Time: time.Now()})
	b, _ := work.Create(beads.Bead{Title: "Human task"})
	first, err := cr.sharedWorkTick(context.Background())
	if err != nil || len(first.Launched) != 1 {
		t.Fatalf("start: %+v, %v", first, err)
	}

	launch := first.Launched[0]
	before, _ := work.Get(b.ID)
	faults := &failingSharedExecutionStore{Store: work, authority: work, failure: "renew"}
	cr.standaloneRigStores["team"] = faults
	if _, err := cr.sharedWorkTick(context.Background()); !errors.Is(err, errSharedInjected) {
		t.Fatalf("renewal failure hidden: %v", err)
	}
	after, _ := work.Get(b.ID)
	if !reflect.DeepEqual(before, after) || provider.CountCalls("Start", launch.SessionName) != 1 {
		t.Fatal("renewal failure wrote/started through a fallback")
	}
	faults.failure = ""
	if err := session.NewStore(beads.SessionStore{Store: sessions}).ApplyPatch(launch.SessionID, map[string]string{"shared_execution_id": ""}); err != nil {
		t.Fatal(err)
	}
	if _, err := cr.sharedWorkTick(context.Background()); !errors.Is(err, beads.ErrExecutionRequired) {
		t.Fatalf("missing original identity was silently ignored/adopted: %v", err)
	}
	after, _ = work.Get(b.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("missing session identity borrowed work ownership")
	}
}

func TestSharedWorkReclaimFailureRefusesFallbackAndLaunch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GC_BEADS", "file")
	rig := t.TempDir()
	clk := &clock.Fake{Time: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
	cr, work, sessions, provider := sharedRuntimeFixture(t, "reclaim", rig, filepath.Join(rig, "work.json"), clk)
	task, err := work.Create(beads.Bead{Title: "Human task"})
	if err != nil {
		t.Fatal(err)
	}
	grant, won, err := work.AcquireExecution(task.ID, beads.ExecutionOwner{CityID: "other-city"}, time.Minute)
	if err != nil || !won {
		t.Fatalf("foreign acquire: %v, %v", won, err)
	}
	clk.Advance(time.Minute)
	before, err := work.InspectExecution(grant)
	if err != nil {
		t.Fatal(err)
	}
	cr.standaloneRigStores["team"] = &failingSharedExecutionStore{Store: work, authority: work, failure: "reclaim"}
	result, err := cr.sharedWorkTick(context.Background())
	if !errors.Is(err, errSharedInjected) || len(result.Reclaimed) != 0 || len(result.Launched) != 0 {
		t.Fatalf("failed reclaim reported success: %+v, %v", result, err)
	}
	after, err := work.InspectExecution(grant)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("reclaim failure revoked foreign work: %+v, %v", after, err)
	}
	rows, err := session.NewStore(beads.SessionStore{Store: sessions}).ListAll(session.ListAllOptions{IncludeClosed: true})
	if err != nil || len(rows) != 0 || sharedRuntimeStartCount(provider) != 0 {
		t.Fatal("failed reclaim launched through a fallback")
	}
}
