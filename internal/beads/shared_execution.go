package beads

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ExecutionIDKey marks an assignment governed by the shared-execution protocol.
const ExecutionIDKey = "gc.execution_id"

// ErrSharedExecutionUnsupported means the store cannot serialize execution
// ownership, launch admission, renewal, reclaim and guarded terminal writes.
// Assignee CAS or ConditionalWriter support alone does not imply this contract.
var ErrSharedExecutionUnsupported = errors.New("shared execution unsupported")

// ErrExecutionRequired refuses a legacy mutation of execution-owned work.
var ErrExecutionRequired = errors.New("shared work requires its original execution grant")

// ErrExecutionLost means the original grant no longer authorizes this bead.
var ErrExecutionLost = errors.New("execution grant is no longer current")

// ErrExecutionAlreadyStarted prevents replaying a grant's launch after a crash
// or an ambiguous provider response. Recovery must acquire a new grant.
var ErrExecutionAlreadyStarted = errors.New("execution launch already consumed")

const (
	executionCityKey     = "gc.execution_city"
	executionDisplayKey  = "gc.execution_display"
	executionDeadlineKey = "gc.execution_lease_until"
	executionTTLKey      = "gc.execution_lease_ttl"
	executionStartedKey  = "gc.execution_started"
	// ExecutionCommentsKey holds an append-only JSON array of execution and
	// human comments. It is separate from human-authored notes/description.
	ExecutionCommentsKey = "gc.execution_comments"
)

// ExecutionGrant is one immutable, never-reused grant, not a session alias.
// Callers retain the original value; reading another owner's token is not
// acquiring authority. This is a cooperative protocol, not a credential.
type ExecutionGrant struct {
	BeadID string `json:"bead_id"`
	ID     string `json:"execution_id"`
}

// ExecutionOwner supplies diagnostic provenance, never eligibility filters.
type ExecutionOwner struct {
	CityID      string
	DisplayName string
}

// ExecutionOperation identifies a single authority-serialized transition.
type ExecutionOperation string

// Execution operations accepted by MutateExecution.
const (
	ExecutionUpdate   ExecutionOperation = "update"
	ExecutionStart    ExecutionOperation = "start"
	ExecutionRenew    ExecutionOperation = "renew"
	ExecutionComplete ExecutionOperation = "complete"
	ExecutionRelease  ExecutionOperation = "release"
)

// ExecutionMutation changes only the original grant's row. Cross-bead
// handoffs, dependencies, parent changes and arbitrary commands are not part
// of this contract and must not be emulated by unconditional writes.
type ExecutionMutation struct {
	Operation     ExecutionOperation
	Update        UpdateOpts
	AppendComment string
	SessionID     string
	SessionName   string
	WorkDir       string
}

// ExecutionComment is an append-only entry. An empty ExecutionID denotes a
// human entry; execution mutations never replace earlier entries.
type ExecutionComment struct {
	ExecutionID string    `json:"execution_id,omitempty"`
	Text        string    `json:"text"`
	CreatedAt   time.Time `json:"created_at"`
}

// SharedExecutionStore is an authority, not a composition of client-side
// Get/CAS calls. Acquisition rechecks ready/open/unassigned under the same
// lock as mutation, renewal and exact-ID reclaim. Launch admission is consumed
// at most once. Release/reclaim/completion clear execution metadata atomically.
//
// Lease expiry makes a grant reclaimable; it does NOT itself revoke authority.
// Renewal may extend an expired-but-unreclaimed lease. Only a committed
// reclaim/release/completion revokes it, serialized against renewal and writes.
// A missing or malformed lease is an error, never permission to reclaim.
//
// Implementing this contract does not claim authentication or protection
// against arbitrary raw Store/CLI/API/SQL/file writes. Those require a separate
// backend/credential boundary; the Enterprise adapter is deliberately refused.
type SharedExecutionStore interface {
	AcquireExecution(id string, owner ExecutionOwner, ttl time.Duration) (ExecutionGrant, bool, error)
	InspectExecution(grant ExecutionGrant) (Bead, error)
	MutateExecution(grant ExecutionGrant, mutation ExecutionMutation) (Bead, error)
	ReclaimExecution(id string) (Bead, bool, error)
}

// SharedExecutionHandleProvider preserves a wrapper's mutation side effects
// while exposing only a proven backing authority.
type SharedExecutionHandleProvider interface {
	SharedExecutionHandle() (SharedExecutionStore, error)
}

// ResolveSharedExecutionStore requires the complete authority contract.
// It never infers lease safety from revision or assignee guards.
func ResolveSharedExecutionStore(store Store) (SharedExecutionStore, error) {
	if provider, ok := store.(SharedExecutionHandleProvider); ok {
		return provider.SharedExecutionHandle()
	}
	if authority, ok := store.(SharedExecutionStore); ok {
		return authority, nil
	}
	return nil, fmt.Errorf("%w: %T has no execution/lease authority", ErrSharedExecutionUnsupported, store)
}

// Validate checks that a grant names one exact bead and execution.
func (g ExecutionGrant) Validate() error {
	if strings.TrimSpace(g.BeadID) == "" || g.BeadID != strings.TrimSpace(g.BeadID) ||
		!strings.HasPrefix(g.ID, "exec-") || len(g.ID) != len("exec-")+64 {
		return ErrExecutionRequired
	}
	for _, c := range strings.TrimPrefix(g.ID, "exec-") {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return ErrExecutionRequired
		}
	}
	return nil
}

// IsExecutionOwned reports whether a row requires execution-aware lifecycle
// handling. A malformed marker is still protected; it is not a legacy claim.
func IsExecutionOwned(b Bead) bool {
	return strings.TrimSpace(b.Metadata[ExecutionIDKey]) != "" ||
		(ExecutionGrant{BeadID: b.ID, ID: b.Assignee}).Validate() == nil
}
