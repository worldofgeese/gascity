package beads

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

var _ SharedExecutionStore = (*MemStore)(nil)

func (m *MemStore) executionTime() time.Time {
	if m.executionNow != nil {
		return m.executionNow().UTC()
	}
	return time.Now().UTC()
}

// AcquireExecution grants one ready, unassigned bead a fresh execution identity.
// The readiness check and write share the same lock as all authority operations.
func (m *MemStore) AcquireExecution(id string, owner ExecutionOwner, ttl time.Duration) (ExecutionGrant, bool, error) {
	if strings.TrimSpace(id) == "" || id != strings.TrimSpace(id) || strings.TrimSpace(owner.CityID) == "" {
		return ExecutionGrant{}, false, ErrExecutionRequired
	}
	if ttl < time.Second || ttl > 24*time.Hour {
		return ExecutionGrant{}, false, fmt.Errorf("execution lease must be between 1s and 24h")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.indexOfLocked(id)
	if i < 0 {
		return ExecutionGrant{}, false, fmt.Errorf("acquiring execution %q: %w", id, ErrNotFound)
	}
	if m.beads[i].Assignee != "" || IsExecutionOwned(m.beads[i]) {
		return ExecutionGrant{}, false, nil
	}
	ready, err := m.readyLocked(context.Background(), ReadyQuery{TierMode: TierBoth})
	if err != nil {
		return ExecutionGrant{}, false, err
	}
	found := false
	for _, b := range ready {
		if b.ID == id {
			found = true
			break
		}
	}
	if !found {
		return ExecutionGrant{}, false, nil
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return ExecutionGrant{}, false, fmt.Errorf("minting execution identity: %w", err)
	}
	grant := ExecutionGrant{BeadID: id, ID: "exec-" + hex.EncodeToString(nonce[:])}
	status := "in_progress"
	m.applyUpdateLocked(i, UpdateOpts{
		Status: &status, Assignee: &grant.ID,
		Metadata: map[string]string{
			ExecutionIDKey:       grant.ID,
			executionCityKey:     owner.CityID,
			executionDisplayKey:  owner.DisplayName,
			executionDeadlineKey: m.executionTime().Add(ttl).Format(time.RFC3339Nano),
			executionTTLKey:      ttl.String(),
			executionStartedKey:  "",
		},
	})
	return grant, true, nil
}

func (m *MemStore) executionIndexLocked(grant ExecutionGrant) (int, error) {
	if err := grant.Validate(); err != nil {
		return -1, err
	}
	i := m.indexOfLocked(grant.BeadID)
	if i < 0 {
		return -1, fmt.Errorf("execution %s: %w", grant.BeadID, ErrNotFound)
	}
	b := m.beads[i]
	if b.Status != "in_progress" || b.Assignee != grant.ID || b.Metadata[ExecutionIDKey] != grant.ID {
		return -1, fmt.Errorf("execution %s: %w", grant.BeadID, ErrExecutionLost)
	}
	if _, _, err := executionLease(b); err != nil {
		return -1, err
	}
	return i, nil
}

func executionLease(b Bead) (time.Time, time.Duration, error) {
	deadline, err := time.Parse(time.RFC3339Nano, b.Metadata[executionDeadlineKey])
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("execution %s has missing or malformed authoritative lease", b.ID)
	}
	ttl, err := time.ParseDuration(b.Metadata[executionTTLKey])
	if err != nil || ttl < time.Second || ttl > 24*time.Hour {
		return time.Time{}, 0, fmt.Errorf("execution %s has invalid authoritative lease duration", b.ID)
	}
	return deadline, ttl, nil
}

// InspectExecution validates the original grant without borrowing current
// ownership from the row. Expiry alone is not revocation.
func (m *MemStore) InspectExecution(grant ExecutionGrant) (Bead, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i, err := m.executionIndexLocked(grant)
	if err != nil {
		return Bead{}, err
	}
	return cloneBead(m.beads[i]), nil
}

func validateExecutionMutation(mutation ExecutionMutation) error {
	switch mutation.Operation {
	case ExecutionStart:
		if strings.TrimSpace(mutation.SessionID) == "" || strings.TrimSpace(mutation.SessionName) == "" {
			return ErrExecutionRequired
		}
	case ExecutionRenew, ExecutionUpdate, ExecutionComplete, ExecutionRelease:
	default:
		return fmt.Errorf("unsupported execution operation %q", mutation.Operation)
	}
	opts := mutation.Update
	if opts.Description != nil {
		return fmt.Errorf("execution mutation preserves human description; append a comment instead")
	}
	if opts.Assignee != nil || opts.Status != nil || opts.ParentID != nil || opts.Type != nil ||
		len(opts.Labels) != 0 || len(opts.RemoveLabels) != 0 {
		return fmt.Errorf("execution mutation refuses ownership, status, type, labels and parent changes; use a guarded terminal operation")
	}
	for key := range opts.Metadata {
		if strings.HasPrefix(key, "gc.") {
			return fmt.Errorf("execution mutation refuses reserved metadata %q", key)
		}
	}
	if mutation.Operation != ExecutionUpdate && mutation.Operation != ExecutionComplete &&
		(opts.Title != nil || opts.Description != nil || opts.Priority != nil || len(opts.Metadata) > 0 || mutation.AppendComment != "") {
		return fmt.Errorf("execution %s does not accept content changes", mutation.Operation)
	}
	return nil
}

func executionCleanupMetadata() map[string]string {
	patch := map[string]string{
		ExecutionIDKey: "", executionCityKey: "", executionDisplayKey: "",
		executionDeadlineKey: "", executionTTLKey: "", executionStartedKey: "",
		beadmeta.SessionIDMetadataKey: "", beadmeta.SessionNameMetadataKey: "",
		beadmeta.WorkDirMetadataKey: "", beadmeta.WorkBranchMetadataKey: "",
	}
	for _, key := range beadmeta.SessionAffinityMetadataKeys {
		patch[key] = ""
	}
	return patch
}

// MutateExecution atomically validates the grant and applies the whole change.
// Renewal updates heartbeat state without changing the issue revision; reclaim
// must therefore serialize with the authority, not rely on a row revision.
func (m *MemStore) MutateExecution(grant ExecutionGrant, mutation ExecutionMutation) (Bead, error) {
	if err := validateExecutionMutation(mutation); err != nil {
		return Bead{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	i, err := m.executionIndexLocked(grant)
	if err != nil {
		return Bead{}, err
	}
	now := m.executionTime()
	opts := mutation.Update
	opts.Metadata = maps.Clone(opts.Metadata)
	if opts.Metadata == nil {
		opts.Metadata = make(map[string]string)
	}
	if mutation.AppendComment != "" {
		var comments []json.RawMessage
		if raw := m.beads[i].Metadata[ExecutionCommentsKey]; raw != "" {
			if err := json.Unmarshal([]byte(raw), &comments); err != nil {
				return Bead{}, fmt.Errorf("appending execution comment: %w", err)
			}
		}
		comment, err := json.Marshal(ExecutionComment{ExecutionID: grant.ID, Text: mutation.AppendComment, CreatedAt: now})
		if err != nil {
			return Bead{}, fmt.Errorf("encoding execution comment: %w", err)
		}
		comments = append(comments, comment)
		encoded, err := json.Marshal(comments)
		if err != nil {
			return Bead{}, fmt.Errorf("encoding execution comments: %w", err)
		}
		opts.Metadata[ExecutionCommentsKey] = string(encoded)
	}
	switch mutation.Operation {
	case ExecutionStart:
		if m.beads[i].Metadata[executionStartedKey] != "" {
			return Bead{}, ErrExecutionAlreadyStarted
		}
		_, ttl, _ := executionLease(m.beads[i])
		opts.Metadata[executionStartedKey] = "true"
		opts.Metadata[executionDeadlineKey] = now.Add(ttl).Format(time.RFC3339Nano)
		opts.Metadata[beadmeta.SessionIDMetadataKey] = mutation.SessionID
		opts.Metadata[beadmeta.SessionNameMetadataKey] = mutation.SessionName
		opts.Metadata[beadmeta.WorkDirMetadataKey] = mutation.WorkDir
	case ExecutionRenew:
		_, ttl, _ := executionLease(m.beads[i])
		m.beads[i].Metadata[executionDeadlineKey] = now.Add(ttl).Format(time.RFC3339Nano)
		return cloneBead(m.beads[i]), nil
	case ExecutionComplete, ExecutionRelease:
		status, assignee := "closed", ""
		if mutation.Operation == ExecutionRelease {
			status = "open"
		}
		opts.Status, opts.Assignee = &status, &assignee
		maps.Copy(opts.Metadata, executionCleanupMetadata())
	}
	m.applyUpdateLocked(i, opts)
	return cloneBead(m.beads[i]), nil
}

// ReclaimExecution checks and revokes exactly one expired grant while holding
// the same lock as renewal. Legacy assignments and live leases are untouched.
func (m *MemStore) ReclaimExecution(id string) (Bead, bool, error) {
	if strings.TrimSpace(id) == "" {
		return Bead{}, false, ErrExecutionRequired
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.indexOfLocked(id)
	if i < 0 {
		return Bead{}, false, fmt.Errorf("reclaiming execution %q: %w", id, ErrNotFound)
	}
	b := m.beads[i]
	if !IsExecutionOwned(b) {
		return cloneBead(b), false, nil
	}
	grant := ExecutionGrant{BeadID: id, ID: b.Metadata[ExecutionIDKey]}
	if _, err := m.executionIndexLocked(grant); err != nil {
		return Bead{}, false, err
	}
	deadline, _, _ := executionLease(b)
	if m.executionTime().Before(deadline) {
		return cloneBead(b), false, nil
	}
	status, assignee := "open", ""
	m.applyUpdateLocked(i, UpdateOpts{
		Status: &status, Assignee: &assignee, Metadata: executionCleanupMetadata(),
	})
	return cloneBead(m.beads[i]), true, nil
}
