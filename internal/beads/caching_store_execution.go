package beads

import "time"

func (c *CachingStore) SharedExecutionHandle() (SharedExecutionStore, error) {
	authority, err := ResolveSharedExecutionStore(c.backing)
	if err != nil {
		return nil, err
	}
	return &cachedExecutionAuthority{cache: c, authority: authority}, nil
}

type cachedExecutionAuthority struct {
	cache     *CachingStore
	authority SharedExecutionStore
}

func (c *cachedExecutionAuthority) AcquireExecution(id string, owner ExecutionOwner, ttl time.Duration) (ExecutionGrant, bool, error) {
	grant, won, err := c.authority.AcquireExecution(id, owner, ttl)
	c.cache.evictForConditionalWrite(id)
	if err != nil || !won {
		return ExecutionGrant{}, false, err
	}
	b, err := c.authority.InspectExecution(grant)
	if err != nil {
		// An ambiguous post-commit read is not launch permission. The original
		// grant remains leased until authoritative recovery; never adopt it.
		return ExecutionGrant{}, false, err
	}
	c.cache.notifyChange("bead.updated", b)
	return grant, true, nil
}

func (c *cachedExecutionAuthority) InspectExecution(grant ExecutionGrant) (Bead, error) {
	return c.authority.InspectExecution(grant)
}

func (c *cachedExecutionAuthority) MutateExecution(grant ExecutionGrant, mutation ExecutionMutation) (Bead, error) {
	b, err := c.authority.MutateExecution(grant, mutation)
	c.cache.evictForConditionalWrite(grant.BeadID)
	if err == nil {
		event := "bead.updated"
		if mutation.Operation == ExecutionComplete {
			event = "bead.closed"
		}
		c.cache.notifyChange(event, b)
	}
	return b, err
}

func (c *cachedExecutionAuthority) ReclaimExecution(id string) (Bead, bool, error) {
	b, reclaimed, err := c.authority.ReclaimExecution(id)
	c.cache.evictForConditionalWrite(id)
	if err == nil && reclaimed {
		c.cache.notifyChange("bead.updated", b)
	}
	return b, reclaimed, err
}
