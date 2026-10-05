package beads

import "time"

var _ SharedExecutionStore = (*FileStore)(nil)

// WithFileStoreExecutionClock supplies the authority's clock. Handles sharing
// a file must use one clock domain (normally the same host's wall clock).
func WithFileStoreExecutionClock(now func() time.Time) FileStoreOption {
	return func(s *FileStore) { s.executionNow = now }
}

func (fs *FileStore) commitExecution(change func() (bool, error)) error {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.locker.Lock(); err != nil {
		return err
	}
	defer fs.locker.Unlock() //nolint:errcheck // best-effort unlock
	if err := fs.reloadFromDisk(); err != nil {
		return err
	}
	snap := fs.snapshotLocked()
	changed, err := change()
	if err != nil {
		fs.restoreFrom(snap.seq, snap.beads, snap.deps)
		return err
	}
	if changed {
		if err := fs.save(); err != nil {
			fs.restoreFrom(snap.seq, snap.beads, snap.deps)
			return err
		}
	}
	return nil
}

// AcquireExecution persists the grant under the cross-process store lock.
func (fs *FileStore) AcquireExecution(id string, owner ExecutionOwner, ttl time.Duration) (ExecutionGrant, bool, error) {
	var grant ExecutionGrant
	var acquired bool
	err := fs.commitExecution(func() (bool, error) {
		var err error
		grant, acquired, err = fs.MemStore.AcquireExecution(id, owner, ttl)
		return acquired, err
	})
	if err != nil {
		return ExecutionGrant{}, false, err
	}
	return grant, acquired, nil
}

// InspectExecution reads the authoritative file rather than a cached snapshot.
func (fs *FileStore) InspectExecution(grant ExecutionGrant) (Bead, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.reloadFromDisk(); err != nil {
		return Bead{}, err
	}
	return fs.MemStore.InspectExecution(grant)
}

// MutateExecution persists grant validation and mutation as one transaction.
func (fs *FileStore) MutateExecution(grant ExecutionGrant, mutation ExecutionMutation) (Bead, error) {
	var b Bead
	err := fs.commitExecution(func() (bool, error) {
		var err error
		b, err = fs.MemStore.MutateExecution(grant, mutation)
		return err == nil, err
	})
	if err != nil {
		return Bead{}, err
	}
	return b, nil
}

// ReclaimExecution serializes lease inspection, revocation and metadata cleanup
// against other processes' renewals and acquisitions.
func (fs *FileStore) ReclaimExecution(id string) (Bead, bool, error) {
	var b Bead
	var reclaimed bool
	err := fs.commitExecution(func() (bool, error) {
		var err error
		b, reclaimed, err = fs.MemStore.ReclaimExecution(id)
		return reclaimed, err
	})
	if err != nil {
		return Bead{}, false, err
	}
	return b, reclaimed, nil
}
