package session

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// SharedExecutionOrigin is the session_origin of sessions started for a shared-work grant.
const (
	SharedExecutionOrigin        = "shared-execution"
	sharedExecutionIDMetadataKey = "shared_execution_id"
	sharedWorkIDMetadataKey      = "shared_work_id"
	sharedWorkScopeMetadataKey   = "shared_work_scope"
)

// SharedExecutionMetadata binds a new session to the ORIGINAL acquired grant.
// These keys are never replaced when another execution takes over the work.
func SharedExecutionMetadata(grant beads.ExecutionGrant, scope string) (map[string]string, error) {
	if err := grant.Validate(); err != nil {
		return nil, err
	}
	if scope == "" || strings.TrimSpace(scope) != scope {
		return nil, beads.ErrExecutionRequired
	}
	return map[string]string{
		"session_origin":                       SharedExecutionOrigin,
		sharedExecutionIDMetadataKey:           grant.ID,
		sharedWorkIDMetadataKey:                grant.BeadID,
		sharedWorkScopeMetadataKey:             scope,
		beadmeta.CurrentClaimBeadIDMetadataKey: grant.BeadID,
	}, nil
}

// IsSharedExecution also protects a damaged partial identity from legacy starts.
func (info Info) IsSharedExecution() bool {
	return info.SessionOrigin == SharedExecutionOrigin ||
		info.SharedExecutionID != "" || info.SharedWorkID != "" || info.SharedWorkScope != ""
}

// ExecutionGrant returns the session's original grant; it fails if the binding is incomplete.
func (info Info) ExecutionGrant() (beads.ExecutionGrant, error) {
	grant := beads.ExecutionGrant{BeadID: info.SharedWorkID, ID: info.SharedExecutionID}
	if info.SharedWorkScope == "" || strings.TrimSpace(info.SharedWorkScope) != info.SharedWorkScope {
		return grant, beads.ErrExecutionRequired
	}
	return grant, grant.Validate()
}

type sharedExecutionStartKey struct{}

type sharedExecutionStart struct {
	grant     beads.ExecutionGrant
	authority beads.SharedExecutionStore
}

// WithSharedExecutionStart supplies launch authority to the worker/session
// boundary. Replaying the context cannot replay the grant's one-shot admission.
func WithSharedExecutionStart(ctx context.Context, grant beads.ExecutionGrant, authority beads.SharedExecutionStore) context.Context {
	return context.WithValue(ctx, sharedExecutionStartKey{}, sharedExecutionStart{grant, authority})
}

func sharedExecutionBinding(ctx context.Context, info Info) (sharedExecutionStart, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	binding, bound := ctx.Value(sharedExecutionStartKey{}).(sharedExecutionStart)
	if !info.IsSharedExecution() && !bound {
		return sharedExecutionStart{}, nil
	}
	grant, err := info.ExecutionGrant()
	if err != nil || !bound || binding.authority == nil || grant != binding.grant {
		return sharedExecutionStart{}, beads.ErrExecutionRequired
	}
	if _, err := binding.authority.InspectExecution(grant); err != nil {
		return sharedExecutionStart{}, fmt.Errorf("validating shared session grant: %w", err)
	}
	return binding, nil
}

func authorizeSharedExecutionStart(ctx context.Context, b beads.Bead) error {
	binding, err := sharedExecutionBinding(ctx, infoFromPersistedBead(b))
	if err != nil || binding.authority == nil {
		return err
	}
	_, err = binding.authority.MutateExecution(binding.grant, beads.ExecutionMutation{
		Operation: beads.ExecutionStart, SessionID: b.ID, SessionName: b.Metadata["session_name"],
		WorkDir: b.Metadata["work_dir"],
	})
	if err != nil {
		return fmt.Errorf("admitting shared session start: %w", err)
	}
	return nil
}

// Provider startup is an external effect, not part of the authority transaction.
// A reclaim committed during startup means the runtime must not be accepted.
func (m *Manager) checkSharedExecutionAfterStart(ctx context.Context, b beads.Bead, name string) error {
	if _, err := sharedExecutionBinding(ctx, infoFromPersistedBead(b)); err != nil {
		return errors.Join(err, m.sp.Stop(name))
	}
	return nil
}
