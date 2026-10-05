package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func persistSharedFixtureConfig(t *testing.T, cr *CityRuntime) {
	t.Helper()
	var encoded bytes.Buffer
	if err := toml.NewEncoder(&encoded).Encode(cr.cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cr.cityPath, "city.toml"), encoded.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY", cr.cityPath)
	t.Setenv("GC_CITY_PATH", cr.cityPath)
	t.Setenv("GC_DIR", cr.cityPath)
	t.Setenv("GC_RIG", "team")
	t.Setenv("GC_CITY_URL", "")
	t.Setenv("GC_CITY_CONTEXT", "")
}

func invokeSharedWorkCommand(args ...string) (string, string, error) {
	var stdout, stderr bytes.Buffer
	command := newSharedWorkCmd(&stdout, &stderr)
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	command.SilenceErrors, command.SilenceUsage = true, true
	command.SetArgs(args)
	err := command.Execute()
	return stdout.String(), stderr.String(), err
}

func bindSharedLaunchEnvironment(t *testing.T, launch sharedWorkLaunch) {
	t.Helper()
	t.Setenv("GC_SHARED_WORK_ID", launch.Grant.BeadID)
	t.Setenv("GC_SHARED_EXECUTION_ID", launch.Grant.ID)
	t.Setenv("GC_SHARED_WORK_SCOPE", "team")
	t.Setenv("GC_SESSION_ID", launch.SessionID)
	t.Setenv("GC_SESSION_NAME", launch.SessionName)
	t.Setenv("GC_AGENT", launch.SessionName)
	t.Setenv("GC_TEMPLATE", "team/worker")
}

func TestSharedWorkCommandsUseRealOpenersAndWorkerLifecycle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GC_BEADS", "file")
	rig := t.TempDir()
	cr, work, sessions, provider := sharedRuntimeFixture(t, "cli-city", rig, filepath.Join(rig, ".gc", "beads.json"), &clock.Fake{Time: time.Now()})
	persistSharedFixtureConfig(t, cr)
	oldProvider := sharedWorkCommandProvider
	sharedWorkCommandProvider = func() (runtime.Provider, error) { return provider, nil }
	t.Cleanup(func() { sharedWorkCommandProvider = oldProvider })
	human, err := work.Create(beads.Bead{
		Title: "Human work", Description: "These human notes must survive",
		Metadata: map[string]string{"human": "keep", beads.ExecutionCommentsKey: `[{"text":"original","author":"Ada"}]`},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, diagnostics, err := invokeSharedWorkCommand("run-once")
	if err != nil {
		t.Fatalf("real CLI run-once: %v\n%s", err, diagnostics)
	}
	var tick sharedWorkTickResult
	if err := json.Unmarshal([]byte(out), &tick); err != nil || len(tick.Launched) != 1 {
		t.Fatalf("run-once result: %s, %v", out, err)
	}
	launch := tick.Launched[0]
	if launch.Grant.BeadID != human.ID || provider.CountCalls("Start", launch.SessionName) != 1 {
		t.Fatalf("CLI did not launch the acquired work: %+v, %+v", launch, provider.Calls)
	}
	bindSharedLaunchEnvironment(t, launch)
	for _, call := range provider.Calls {
		if call.Method == "Start" && !strings.Contains(call.Config.Nudge, "gc work complete") {
			t.Fatalf("worker did not receive reachable guarded work instructions: %+v", call.Config)
		}
	}
	for _, args := range [][]string{
		{"show"},
		{"comment", "first execution note"},
		{"update", "--title", "Updated task", "--metadata", "result=verified", "--comment", "second note"},
		{"renew"},
	} {
		if _, diagnostics, err := invokeSharedWorkCommand(args...); err != nil {
			t.Fatalf("gc work %v: %v\n%s", args, err, diagnostics)
		}
	}
	var hookOut, hookErr bytes.Buffer
	if code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &hookOut, &hookErr); code != 0 {
		t.Fatalf("real shared hook: code=%d %s", code, hookErr.String())
	}
	var hook hookClaimJSONResult
	if err := json.Unmarshal(hookOut.Bytes(), &hook); err != nil || hook.Action != "work" || hook.Assignee != launch.Grant.ID || hook.BeadID != human.ID {
		t.Fatalf("shared hook adopted wrong identity: %+v %v", hook, err)
	}
	hookOut.Reset()
	hookErr.Reset()
	if code := cmdHookCurrent(true, &hookOut, &hookErr); code != 0 || hookOut.String() != human.ID+"\n" {
		t.Fatalf("real current hook = %d %q %s", code, hookOut.String(), hookErr.String())
	}
	before, _ := work.InspectExecution(launch.Grant)
	for _, args := range [][]string{
		{"update", "--metadata", "gc.execution_id=borrowed"},
		{"update", "--metadata", beads.ExecutionCommentsKey + "=[]"},
		{"update", "--metadata", "not-a-pair"},
		{"update", "--description", "replace human notes"},
		{"complete", human.ID},
		{"reclaim", human.ID + "*"},
	} {
		if _, _, err := invokeSharedWorkCommand(args...); err == nil {
			t.Fatalf("unsafe CLI command accepted: %v", args)
		}
	}
	after, _ := work.InspectExecution(launch.Grant)
	if before.Revision != after.Revision || before.Metadata[beads.ExecutionCommentsKey] != after.Metadata[beads.ExecutionCommentsKey] {
		t.Fatal("refused CLI command changed work")
	}
	t.Setenv("GC_SHARED_EXECUTION_ID", "")
	if _, _, err := invokeSharedWorkCommand("complete"); err == nil {
		t.Fatal("completion without original identity succeeded")
	}
	if code := doHookCurrent(session.NewStore(beads.SessionStore{Store: sessions}), launch.SessionID, true, &hookOut, &hookErr); code != 1 {
		t.Fatal("legacy hook current exposed an unvalidated shared claim")
	}
	t.Setenv("GC_SHARED_EXECUTION_ID", launch.Grant.ID)
	if code := doBd([]string{"update", human.ID, "--status=closed"}, &hookOut, &hookErr); code != 1 {
		t.Fatal("shared gc bd forwarding was not refused")
	}
	sling := newSlingCmd(&hookOut, &hookErr)
	sling.SetArgs([]string{"team/worker", human.ID})
	sling.SilenceErrors, sling.SilenceUsage = true, true
	if err := sling.Execute(); err == nil {
		t.Fatal("shared execution was allowed legacy sling")
	}
	if _, diagnostics, err := invokeSharedWorkCommand("complete"); err != nil {
		t.Fatalf("guarded CLI completion: %v %s", err, diagnostics)
	}
	closed, err := work.Get(human.ID)
	if err != nil || closed.Status != "closed" || closed.Assignee != "" || closed.Description != human.Description ||
		closed.Metadata["human"] != "keep" || closed.Metadata["result"] != "verified" || closed.Metadata[beads.ExecutionIDKey] != "" {
		t.Fatalf("CLI completion damaged content/cleanup: %+v, %v", closed, err)
	}
	var comments []map[string]any
	if err := json.Unmarshal([]byte(closed.Metadata[beads.ExecutionCommentsKey]), &comments); err != nil ||
		len(comments) != 3 || comments[0]["author"] != "Ada" {
		t.Fatalf("human comments replaced: %+v, %v", comments, err)
	}
	if _, _, err := invokeSharedWorkCommand("complete"); err == nil {
		t.Fatal("terminal grant replay was reported successful")
	}
	hookOut.Reset()
	hookErr.Reset()
	if code := cmdHookCurrent(true, &hookOut, &hookErr); code != 1 || hookOut.Len() != 0 {
		t.Fatal("current hook exposed a completed grant")
	}
	for _, key := range []string{"GC_SHARED_WORK_ID", "GC_SHARED_EXECUTION_ID", "GC_SHARED_WORK_SCOPE", "GC_SESSION_ID"} {
		t.Setenv(key, "")
	}
	if _, diagnostics, err := invokeSharedWorkCommand("run-once"); err != nil || provider.IsRunning(launch.SessionName) {
		t.Fatalf("CLI did not retire completed worker: %v %s", err, diagnostics)
	}
	if provider.CountCalls("Start", launch.SessionName) != 1 {
		t.Fatal("CLI restarted an old grant")
	}
}

func TestSharedWorkFileModeRefusesPersonalLedgerFallback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GC_BEADS", "file")
	rig := t.TempDir()
	path := filepath.Join(rig, ".gc", "beads.json")
	cr, _, sessions, _ := sharedRuntimeFixture(t, "missing-common-store", rig, path, &clock.Fake{Time: time.Now()})
	persistSharedFixtureConfig(t, cr)
	local, err := sessions.Create(beads.Bead{Title: "City-local work, not the shared pool"})
	if err != nil {
		t.Fatal(err)
	}
	// No common store has been persisted yet. Opening a shared rig must not
	// take openCompatibleFileStore's historical city-ledger alias.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("fixture unexpectedly persisted common store: %v", err)
	}
	state, err := openSharedWorkCommandContext(&bytes.Buffer{})
	if state != nil || err == nil {
		t.Fatalf("missing common store silently opened the personal ledger: %v, %v", state, err)
	}
	after, err := sessions.Get(local.ID)
	if err != nil || after.Assignee != "" || after.Status != "open" {
		t.Fatalf("personal work was changed: %+v, %v", after, err)
	}
}

func TestSharedWorkConfigModeRefusesLegacyBdWithoutGrant(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GC_BEADS", "file")
	for _, key := range []string{"GC_SHARED_WORK_ID", "GC_SHARED_EXECUTION_ID", "GC_SHARED_WORK_SCOPE"} {
		t.Setenv(key, "")
	}
	rig := t.TempDir()
	cr, _, _, _ := sharedRuntimeFixture(t, "no-bd", rig, filepath.Join(rig, "work.json"), &clock.Fake{Time: time.Now()})
	persistSharedFixtureConfig(t, cr)
	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"sql", "update issues set status='closed'"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "shared-work mode") {
		t.Fatalf("unbound shared bd was not refused before passthrough: %d %s", code, stderr.String())
	}
}

func TestSharedWorkStaleCLIAndHookCannotTouchSuccessor(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GC_BEADS", "file")
	clk := &clock.Fake{Time: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
	rig := t.TempDir()
	path := filepath.Join(rig, ".gc", "beads.json")
	oldCity, work, _, oldProvider := sharedRuntimeFixture(t, "old-city", rig, path, clk)
	newCity, successorWork, _, newProvider := sharedRuntimeFixture(t, "new-city", rig, path, clk)
	persistSharedFixtureConfig(t, oldCity)
	task, err := work.Create(beads.Bead{Title: "Human task", Metadata: map[string]string{"human": "keep"}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := oldCity.sharedWorkTick(context.Background())
	if err != nil || len(first.Launched) != 1 {
		t.Fatalf("first launch: %+v, %v", first, err)
	}
	old := first.Launched[0]
	bindSharedLaunchEnvironment(t, old)
	// A stopped process may resume after another city's authoritative recovery.
	// Neither a repeated display/template name nor the new row's token can
	// replace the old session's original binding.
	if err := oldProvider.Stop(old.SessionName); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Minute)
	takeover, err := newCity.sharedWorkTick(context.Background())
	if err != nil || len(takeover.Launched) != 1 || len(takeover.Reclaimed) != 1 {
		t.Fatalf("takeover: %+v, %v", takeover, err)
	}
	current := takeover.Launched[0]
	if old.Grant == current.Grant {
		t.Fatal("recovery reused the original grant")
	}
	before, err := successorWork.InspectExecution(current.Grant)
	if err != nil {
		t.Fatal(err)
	}
	beforeBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"show"},
		{"update", "--title", "stale"},
		{"comment", "stale"},
		{"complete"},
		{"release"},
		{"renew"},
	} {
		if _, _, err := invokeSharedWorkCommand(args...); !errors.Is(err, beads.ErrExecutionLost) {
			t.Fatalf("stale gc work %v did not refuse original grant: %v", args, err)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr); code != 1 {
		t.Fatalf("stale unacknowledged hook must drain with nonzero exit: %d %s", code, stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || result.Action != "drain" {
		t.Fatalf("stale hook result: %s, %v", stdout.String(), err)
	}
	stdout.Reset()
	if code := cmdHookCurrent(true, &stdout, &stderr); code != 1 || stdout.Len() != 0 {
		t.Fatal("stale current hook exposed successor work")
	}
	t.Setenv("GC_SHARED_EXECUTION_ID", current.Grant.ID)
	if _, _, err := invokeSharedWorkCommand("release"); !errors.Is(err, beads.ErrExecutionRequired) {
		t.Fatalf("old session borrowed successor token: %v", err)
	}
	t.Setenv("GC_SHARED_EXECUTION_ID", old.Grant.ID)
	if _, _, err := invokeSharedWorkCommand("reclaim", task.ID); err == nil {
		t.Fatal("old execution reached the operator-only recovery command")
	}
	if _, _, err := invokeSharedWorkCommand("run-once"); err == nil {
		t.Fatal("old execution reached the operator-only controller command")
	}
	after, err := successorWork.InspectExecution(current.Grant)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("stale CLI changed successor: %+v, %v", after, err)
	}
	afterBytes, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(beforeBytes, afterBytes) {
		t.Fatalf("refused CLI path wrote the shared file: %v", err)
	}
	if !newProvider.IsRunning(current.SessionName) || sharedRuntimeStartCount(oldProvider) != 1 {
		t.Fatal("stale command stopped successor or restarted its old execution")
	}
	persistSharedFixtureConfig(t, newCity)
	bindSharedLaunchEnvironment(t, current)
	if _, _, err := invokeSharedWorkCommand("release"); err != nil {
		t.Fatalf("current successor could not release through the real CLI: %v", err)
	}
	released, err := work.Get(task.ID)
	if err != nil || released.Status != "open" || released.Assignee != "" ||
		released.Metadata[beads.ExecutionIDKey] != "" || released.Metadata["human"] != "keep" {
		t.Fatalf("guarded CLI release did not clear ownership while keeping content: %+v, %v", released, err)
	}
}
