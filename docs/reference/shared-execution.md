---
title: "Shared-Work Execution Authority"
---

The Bead is the WHAT primitive and the Rig is the WHERE — see
[How Gas City Works](/getting-started/how-gas-city-works) for where they sit
among the six. Shared-work mode lets several cities draw ready beads from one
rig's store as a common work pool. Each successful acquisition gets one fresh
**execution grant**: the orchestrator uses that grant to start one worker,
renew its lease, and fence the worker's mutations to that bead. Recovery is an
exact-ID authority operation, not a conclusion drawn from another city's
missing session.

This is an opt-in **cooperative native-store protocol**, not a hosted Enterprise
rollout or an authentication boundary.

## Why

Without it, each city schedules only its own work. Pointing several cities at
one store and letting each claim work by assignee leaves two problems: a city
that cannot see another city's session cannot tell whether that work is
abandoned, and a stale worker can overwrite its successor. Shared-work mode
makes the store's execution grant the single authority for who may run and
mutate a bead, so cities can race for the same pool and recover each other's
expired work safely.

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

Field types and defaults are in the
[configuration reference](/reference/config).

The rig and its worker template must already exist. Bind each participating
city's rig to the same native rig root using the normal site configuration.
The common `<rig-root>/.gc/beads.json` must already exist: shared mode refuses
the legacy file opener's fallback to the city's personal ledger. It does not
initialize, copy, migrate, or partition team work.

The session ledger stays city-local. Do not route a city's session ledger into
the common work store. Different paths to different files are different
stores, not a distributed shared pool.

`template` selects worker behavior, not work eligibility. All normal ready,
open, unassigned work in the selected rig participates, without creator,
label, priority-band, person, or city-route restrictions. Normal dependency,
defer, and infrastructure exclusions still apply. Assigned and closed work is
not stolen.

The default lease is two minutes; accepted durations are one second through
24 hours. Choose a lease longer than the expected reconciliation and startup
delays. `max_active` is a per-city active-execution limit, defaulting to one,
not a partition of the work pool. Zero means the default; negative limits are
invalid.

The selected template cannot also back a configured named session. Workers
must use the long-lived lifecycle: `lifecycle = "one_shot"` is refused before
acquisition. Existing suspension rules still prevent new acquisitions.

Without `[beads.shared_work]`, ordinary scheduling is unchanged. Execution
markers remain protected from legacy cleanup even if a city removes the
selection; removing the configuration is not a release or recovery operation.

## What the orchestrator does

On boot and on each reconciliation pass, the orchestrator:

1. Checks the grant behind each of this city's shared workers. It stops only
   the worker whose grant was revoked or whose work disappeared. A store error
   is treated as an error, not as proof of revocation.
2. Renews grants for workers it can see running and that are not suspended.
3. Asks the store to reclaim each execution-owned bead whose lease has
   expired. The store decides; the orchestrator never judges a lease itself.
4. Races other cities for ready work. Only a confirmed acquisition starts a
   worker.
5. Checks the grant again after the worker starts, and stops a worker whose
   grant was revoked during startup.

`gc work run-once` runs one such pass without starting the orchestrator loop.
It can start real workers.

A grant contains the bead ID and a fresh 256-bit random execution ID. The full
execution ID becomes the bead's assignee and the worker's `BEADS_ACTOR`. The
city name and template are diagnostic labels, never authority. Worker sessions
get unique runtime names per grant rather than reused display names.

Each worker receives its binding as `GC_SHARED_WORK_ID`,
`GC_SHARED_EXECUTION_ID`, and `GC_SHARED_WORK_SCOPE`. Worker commands check
that environment against the session's original binding; they never adopt the
current bead's token. Missing or malformed identity refuses before launch or
mutation.

Shared workers are left alone by ordinary pool scaling, stale-session cleanup,
and session respawn. A stopped execution cannot restart from its spent grant.
An ambiguous acquisition or start is not retried under the same grant or
force-released; authoritative recovery eventually makes a fresh acquisition
possible.

## Guarded worker commands

These commands operate only on the calling session's original work. Full flags
are in the [CLI reference](/reference/cli).

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
legacy claim, adoption, or continuation logic. A revoked grant yields a
terminal drain result (nonzero until acknowledged, matching the hook contract).
`gc hook current` also validates authority. `gc bd` and `gc sling` refuse
selected shared mode or a marked shared execution rather than forwarding unsafe
writes.

Outside a worker session, `gc work reclaim EXACT_ID` asks the authority to
reclaim one expired grant. It never expands an ID to a backlog query.
`reclaim` and `run-once` refuse to run from inside a worker session: they are
operator operations, not worker mutations.

## Lease semantics

Expiry makes a grant **eligible for reclaim**; it does not itself revoke it.
Renewal may extend an expired but unreclaimed grant. Only a committed reclaim,
release, or completion revokes authority.

Renewal and reclaim are serialized by the store. If renewal commits first,
reclaim sees the extended lease; if reclaim commits first, the old execution
cannot renew, update, complete, release, or clear its successor's metadata.
Missing or malformed lease state is an error, never permission to reclaim.

## Supported stores and limits

| Store | Status |
| --- | --- |
| In-memory store | Supported; used for conformance tests. |
| `file` provider | Supported. Persistent, using the existing file lock, reload, atomic write/rename, and rollback behavior. |
| Cache and GC policy wrappers | Supported over a supported backing store; cached rows are invalidated on every grant change, including renewal. |
| `bd` / Enterprise, SQLite, and other stores | Refused with an explicit unsupported error. No legacy or unconditional fallback. |

`file` provider support has been exercised with independent handles on a local
filesystem sharing one clock. It does not establish locking across hosts or
network filesystems, tolerance for clock skew, or stronger power-loss
durability than the existing store.

Starting a worker is an external effect, not part of the store transaction. A
worker process can briefly exist during a concurrent reclaim; the post-start
check stops it, and its guarded writes stay fenced. This does not fence
arbitrary external side effects.

Grant IDs are not credentials. Raw store, API, SQL, or file writes, raw `bd`,
or a caller copying another execution's token bypass the cooperative
discipline. Mandatory backend enforcement and authenticated lease ownership
need a separate verified backend contract. Neither assignee guards nor the
existing revision-conditional writes are treated as that contract. No
Enterprise revision flags, replica identities, or lease guarantees are
inferred.
