package main

import "github.com/gastownhall/gascity/internal/beads"

func (s *beadPolicyStore) SharedExecutionHandle() (beads.SharedExecutionStore, error) {
	return beads.ResolveSharedExecutionStore(s.Store)
}
