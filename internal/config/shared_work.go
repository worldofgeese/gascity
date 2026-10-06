package config

import (
	"fmt"
	"strings"
	"time"
)

// SharedWorkConfig deliberately selects execution-authority-based scheduling.
// Rig names the common work store; Template selects worker behavior, never
// creator, priority, label or routing eligibility.
type SharedWorkConfig struct {
	// Rig names the rig whose store holds the common work pool.
	Rig string `toml:"rig"`
	// Template is the worker template started for each acquired execution.
	// It selects worker behavior, not which work is eligible.
	Template string `toml:"template"`
	// Lease is the execution grant's lease TTL as a Go duration, from 1s
	// to 24h. Empty defaults to 2m.
	Lease string `toml:"lease,omitempty" jsonschema:"default=2m"`
	// MaxActive caps this city's concurrently active executions. Zero or
	// unset defaults to 1; negative values are invalid.
	MaxActive int `toml:"max_active,omitempty" jsonschema:"default=1,minimum=0"`
}

// LeaseDuration returns the lease TTL (default 2m), bounded to 1s..24h.
func (s SharedWorkConfig) LeaseDuration() (time.Duration, error) {
	raw := s.Lease
	if raw == "" {
		raw = "2m"
	}
	ttl, err := time.ParseDuration(raw)
	if err != nil || ttl < time.Second || ttl > 24*time.Hour {
		return 0, fmt.Errorf("beads.shared_work.lease must be between 1s and 24h, got %q", raw)
	}
	return ttl, nil
}

// ActiveLimit returns MaxActive, defaulting to 1.
func (s SharedWorkConfig) ActiveLimit() int {
	if s.MaxActive == 0 {
		return 1
	}
	return s.MaxActive
}

// Validate requires an exact rig and template, a non-negative MaxActive and a valid lease.
func (s SharedWorkConfig) Validate() error {
	if strings.TrimSpace(s.Rig) == "" || s.Rig != strings.TrimSpace(s.Rig) ||
		strings.TrimSpace(s.Template) == "" || s.Template != strings.TrimSpace(s.Template) {
		return fmt.Errorf("beads.shared_work requires an exact rig and worker template")
	}
	if s.MaxActive < 0 {
		return fmt.Errorf("beads.shared_work.max_active must not be negative")
	}
	_, err := s.LeaseDuration()
	return err
}
