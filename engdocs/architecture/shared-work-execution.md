---
title: "Shared-Work Execution"
---

> Last verified against code: 2026-10-06

## Summary

Shared-work mode (`[beads.shared_work]`) lets several cities schedule from one
rig's native work store. A store-side execution grant, not the assignee or a
local session, is the authority for who may run and mutate a bead. This doc
covers how the controller drives that protocol and where the code lives. The
operator-facing contract (configuration, `gc work` commands, lease semantics,
supported stores) is the
[Shared-Work Execution Authority reference](../../docs/reference/shared-execution.md).

## Key code

| Concern | Location |
| --- | --- |
| Configuration and validation | `internal/config/shared_work.go` (`SharedWorkConfig`) |
| Authority contract | `internal/beads/shared_execution.go` (`SharedExecutionStore`, `ExecutionGrant`, `ResolveSharedExecutionStore`) |
| Native authorities | `internal/beads/memstore_execution.go`, `internal/beads/filestore_execution.go` |
| Cache wrapper | `internal/beads/caching_store_execution.go` (`CachingStore.SharedExecutionHandle`) |
| Session binding and start admission | `internal/session/shared_execution.go` |
| Controller coordinator | `cmd/gc/shared_work_runtime.go` (`sharedWorkRuntime`, `CityRuntime.sharedWorkTick`) |
| Guarded worker commands | `cmd/gc/cmd_work.go` |

`ResolveSharedExecutionStore` requires the complete four-method authority
(`AcquireExecution`, `InspectExecution`, `MutateExecution`,
`ReclaimExecution`). Stores without it return
`ErrSharedExecutionUnsupported`; lease safety is never inferred from revision
or assignee guards.

## Reconcile lifecycle

`CityRuntime.beadReconcileTick` (`cmd/gc/city_runtime.go`) calls
`CityRuntime.sharedWorkTick` on boot and on each later reconciliation pass.
`gc work run-once` runs the same coordinator without a controller loop. Each
`sharedWorkRuntime.tick`:

1. Reads this city's sessions through the city-local session store and keeps
   each session's original work ID, grant ID, and rig scope.
2. Calls `InspectExecution` on each original grant. It stops and closes only
   that execution's uniquely named local worker if the grant was revoked or the
   work disappeared. Backend uncertainty is an error, not proof of revocation.
3. Renews grants (`MutateExecution` with `ExecutionRenew`) for positively
   observed, nonsuspended local workers.
4. Calls `ReclaimExecution` for each exact execution-owned work ID. The
   authority evaluates the lease; the controller's row snapshot supplies no
   lease verdict.
5. Races for ready work with `AcquireExecution`. Only a confirmed new
   acquisition proceeds, through `worker.Handle.Create` and the session manager
   (`sharedWorkRuntime.acquireAndStart`).
6. Consumes the grant's one-shot start admission immediately before provider
   startup (`authorizeSharedExecutionStart`), then checks authority again after
   startup (`Manager.checkSharedExecutionAfterStart`) and stops an execution
   revoked during startup.

The original binding is persisted on the session bead
(`SharedExecutionMetadata`) and injected as `GC_SHARED_WORK_ID`,
`GC_SHARED_EXECUTION_ID`, and `GC_SHARED_WORK_SCOPE`; `BEADS_ACTOR` is the full
execution ID. Worker commands cross-check that environment against the
session binding and never adopt the current work row's token.

## Isolation from legacy lifecycle

Shared sessions are excluded from ordinary pool sweep, stale-create and
pre-boot cleanup, and session respawn reconciliation. The selected template is
also excluded from legacy demand and minimum-pool allocation. Legacy
work-directory, session/root stamping, and route repairs skip execution-owned
snapshots (`beads.IsExecutionOwned`) instead of overwriting their execution
metadata. A stopped execution cannot restart from its consumed grant.
Ambiguous acquire, admission, or start results are not retried as a new grant
or unconditionally released; authoritative recovery eventually makes a fresh
acquisition possible.

## Invariants

- Renewal and reclaim serialize on the same authority lock or transaction.
  Renewal changes the lease without advancing the issue revision, so a
  client-side lease read followed by a revision compare-and-swap would not
  implement the contract.
- The cache wrapper exposes only a proven backing authority and invalidates
  cached rows on every grant transition, including revision-unchanged
  renewal.
- An absent local session is never permission to release an execution-owned
  row.
- The session ledger stays city-local; only the work store is shared.

Requirement IDs `SESSION-SHARED-001` through `SESSION-SHARED-007` in
[`internal/session/REQUIREMENTS.md`](../../internal/session/REQUIREMENTS.md)
state these as testable scenarios.

## Test evidence

- `cmd/gc/shared_work_controller_test.go` exercises boot and steady-state
  controller dispatch and races two independent city/session contexts against
  one native file store, then exercises renewal, crash takeover, old session
  retirement, and completion through real worker handles.
- `cmd/gc/cmd_work_test.go` uses real configuration and store openers and
  Cobra/hook commands, and proves stale commands leave the successor's
  persisted work file unchanged.
- `cmd/gc/shared_work_failures_test.go` injects backend and response failures
  around real native authority calls: ambiguous acquisition and admission,
  restart, missing identity, and unsupported capability.
- `internal/beads/shared_execution*_test.go` and
  `internal/session/shared_execution*_test.go` cover native transitions, cache
  behavior, human content, one-shot admission, and revocation during provider
  startup.

Runtime providers are fakes: these tests launch no model agents and connect to
no hosted database. Failure injection and the deterministic lease clock are
test controls; the native authority, file persistence, session ledger, worker
boundary, configuration opening, and guarded commands are production code.
