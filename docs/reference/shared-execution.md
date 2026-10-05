---
title: "Shared-Work Execution Authority"
---

Shared-work mode gives each successful acquisition one fresh execution grant.
The controller uses that grant to start one worker, renew its lease, and fence
work mutations. Recovery is an exact-ID authority operation, not a conclusion
drawn from another city's missing session.

This is an opt-in **cooperative native-store protocol**, not a hosted Enterprise
rollout or an authentication boundary.

## Selection and storage

Add an explicit selection to an otherwise configured city:

```toml
[beads]
provider = "file"

[beads.shared_work]
rig = "team"
template = "team/worker"
lease = "2m"
max_active = 1
```

The rig and its worker template must already exist. Bind each participating
city's rig to the same native rig root using the normal site configuration.
The common `<rig-root>/.gc/beads.json` must already exist: shared mode refuses
the legacy file opener's fallback to the city's personal ledger. It does not
initialize, copy, migrate, or partition team work.

The session ledger remains city-local in the exercised configuration; session
operations use the existing typed session-store routing. Do not route a city's
session ledger into the common work store. Different paths to different files
are different stores, not a distributed shared pool.

`template` selects worker behavior, not work eligibility. All normal ready,
open, unassigned work in the selected rig participates, without creator,
label, priority-band, person, or city-route restrictions. Normal dependency,
defer, and infrastructure exclusions in `Ready` still apply. Assigned and
closed work is not stolen.

The default lease is two minutes; accepted durations are one second through
24 hours. Choose a lease longer than the expected reconciliation/startup
delays. `max_active` is a per-city active-execution limit, defaulting to one,
not a partition of the work pool. Negative limits are invalid.

The selected template cannot also back a configured named session. Workers
must use the long-lived lifecycle: `lifecycle = "one_shot"` is refused before
acquisition. Existing suspension rules still prevent new acquisitions.

Without `[beads.shared_work]`, ordinary scheduling is unchanged. Execution
markers remain protected from legacy cleanup even if a city removes the
selection; removing the configuration is not a release/recovery operation.

## Reachable lifecycle

`CityRuntime.beadReconcileTick` invokes the shared coordinator on boot and
subsequent reconciliation passes. `gc work run-once` exercises the same
coordinator without starting a controller loop; it can start real workers.

1. Read this city's sessions through `session.Store` and retain each original
   work ID, grant ID, and rig scope.
2. Inspect each original grant. Stop/close only that execution's uniquely named
   local worker if its grant was revoked or the work disappeared. Backend
   uncertainty is an error, not proof of revocation.
3. Renew grants for positively observed, nonsuspended local workers.
4. Ask the authority to reclaim each exact execution-owned work ID. The
   authority evaluates the lease; the controller's row snapshot supplies no
   lease verdict.
5. Race for ready work. Only a confirmed new acquisition proceeds through the
   existing `worker.Handle.Create` and session manager.
6. Consume the grant's one-shot start admission immediately before provider
   startup. Check authority again after startup; stop and reject an execution
   revoked during startup.

The city identifier and template/display name are diagnostic provenance, never
authority. A grant contains the work ID and a fresh 256-bit random execution ID.
The full execution ID becomes the work assignee. Local runtime names are unique
to the grant rather than reused display names.

The original binding is persisted on the session and injected as
`GC_SHARED_WORK_ID`, `GC_SHARED_EXECUTION_ID`, and `GC_SHARED_WORK_SCOPE`.
`BEADS_ACTOR` is the full execution ID. Worker commands cross-check that launch
environment against the original session binding; they never adopt the current
work row's token. Missing or malformed identity refuses before launch/mutation.

Shared sessions are excluded from ordinary pool sweep, stale-create/pre-boot
cleanup, and session respawn reconciliation. The selected template is also
excluded from legacy demand and minimum-pool allocation. Legacy work-directory,
session/root stamping, and route repairs skip execution-owned snapshots instead
of overwriting their execution metadata. A stopped execution cannot restart from
its consumed grant. Ambiguous acquire/admission/start results are not retried as
a new grant or unconditionally released; authoritative recovery eventually
makes a fresh acquisition possible.

## Guarded worker commands

These commands operate only on the calling session's original work:

| Command | Effect |
| --- | --- |
| `gc work show` | Inspect current authority and return the task. |
| `gc work update --title TEXT --priority N --metadata key=value --comment TEXT` | Change explicitly supplied content under the original grant. |
| `gc work comment TEXT` | Append progress without replacing existing human context. |
| `gc work renew` | Renew the original grant through the authority. |
| `gc work complete` | Close work and clear its active execution/session metadata atomically. |
| `gc work release` | Reopen work and clear its active execution/session metadata atomically. |

Execution updates cannot change assignee, status, type, labels, parents,
description, or reserved `gc.*` metadata. Cross-bead handoffs, graph changes,
and arbitrary commands have no guarded implementation in this protocol.

The generic store does not expose a notes field. Comments are appended to the
JSON array in `gc.execution_comments`; existing array entries, including unknown
human-authored fields, survive. Description is preserved. Completion, release,
and reclaim keep unrelated metadata and comment history while clearing grant,
lease, session binding, work-directory/branch, and session-affinity metadata.
This is not an adapter for Enterprise `--append-notes`.

`gc hook --claim` retrieves the original authorized task rather than running
legacy claim/adoption/continuation logic. A revoked grant yields a terminal
drain result (nonzero until acknowledged, matching the hook contract).
`gc hook current` also validates authority. `gc bd` and `gc sling` refuse selected
shared mode or a marked shared execution rather than forwarding unsafe writes.

Outside an execution environment, `gc work reclaim EXACT_ID` asks the authority
to reclaim one expired grant. It never expands an ID to a backlog query.
`reclaim` and `run-once` refuse a calling session's runtime identity: they are
controller/operator operations, not worker mutations.

## Lease semantics

Expiry makes a grant **eligible for reclaim**; it does not itself revoke it.
Renewal may extend an expired but unreclaimed grant. Only a committed reclaim,
release, or completion revokes authority.

Renewal and reclaim serialize on the same authority lock/transaction. Renewal
changes the lease without advancing the issue revision, so a client-side lease
read followed by revision CAS would not implement this contract. If renewal
commits first, reclaim observes the extended lease; if reclaim commits first,
the old execution cannot renew, update, complete, release, or clear successor
metadata. Missing or malformed authoritative lease state is an error, never
permission to reclaim.

## Supported and unverified boundaries

| Surface | Status |
| --- | --- |
| `MemStore` | Mutex-serialized native authority; used for conformance tests. |
| `FileStore` | Persistent native authority using the existing file lock, reload, atomic write/rename, and rollback behavior. |
| Cache and GC policy wrappers | Preserve a proven backing authority and invalidate cached rows, including revision-unchanged renewal. |
| `BdStore` / Enterprise, SQLite, or other wrappers without the complete capability | Explicit `ErrSharedExecutionUnsupported`; no legacy or unconditional fallback. |

FileStore evidence uses independent handles on a local filesystem and a common
clock domain. It does not establish multi-host/network-filesystem locking,
clock-skew tolerance, or stronger power-loss durability than the existing store.

Provider startup is an external effect, not part of the work-store transaction.
A process can briefly exist during a concurrent reclaim; post-start validation
rejects/stops it, and guarded work writes remain fenced. This does not fence
arbitrary external side effects.

Grant IDs are not credentials. Raw store/API/SQL/file writes, raw `bd`, or a
caller copying another execution's token bypass cooperative discipline.
Mandatory backend enforcement and authenticated lease ownership need a separate
verified backend contract. Neither assignee guards nor the existing revision
conditional-writer capability are treated as that contract. No Enterprise
revision flags, replica identities, or lease guarantees are inferred.

## Local evidence

`shared_work_controller_test.go` exercises actual boot/steady-state controller
dispatch and races two independent city/session contexts against one native file
store, then exercises renewal, crash takeover, old session retirement, and
completion through real worker handles.
`cmd_work_test.go` uses real configuration/store openers and Cobra/hook commands,
and proves stale commands leave the successor's persisted work file unchanged.

`shared_work_failures_test.go` injects backend/response failures around real
native authority calls; it covers ambiguous acquisition/admission, restart,
missing identity, and unsupported capability. `internal/beads/shared_execution*`
and `internal/session/shared_execution*` cover native transitions, cache behavior,
human content, one-shot admission, and revocation during provider startup.

Runtime providers are fakes: these tests launch no model agents and connect to
no hosted database. Failure injection and the deterministic lease clock are
test controls; the native authority, file persistence, session ledger, worker
boundary, configuration opening, and guarded commands are production code.
