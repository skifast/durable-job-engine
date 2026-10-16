// Package config reads the job-type file the engine loads at startup.
//
// The file is JSON (the Go standard library reads it with no outside package).
// Example:
//
//	{
//	  "job_types": {
//	    "send_email":   {},
//	    "wire_payment": {"needs_approval": true, "approval_window": "24h",
//	                     "handler_time_limit": "60s", "max_attempts": 3}
//	  }
//	}
//
// Anything left out takes its default. A new job type needs a restart.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

const (
	DefaultApprovalWindow   = 24 * time.Hour
	DefaultHandlerTimeLimit = 60 * time.Second
	DefaultMaxAttempts      = 8
	MinMaxAttempts          = 1
	MaxMaxAttempts          = 100
)

// JobType is the setting for one job type, after defaults are applied.
type JobType struct {
	NeedsApproval    bool
	ApprovalWindow   time.Duration // only used when NeedsApproval
	HandlerTimeLimit time.Duration
	MaxAttempts      int
}

// Config maps a job type name to its settings.
type Config struct {
	JobTypes map[string]JobType
}

// rawJobType is the file's shape. Pointers tell "left out" apart from "set to zero".
type rawJobType struct {
	NeedsApproval    bool    `json:"needs_approval"`
	ApprovalWindow   *string `json:"approval_window"`
	HandlerTimeLimit *string `json:"handler_time_limit"`
	MaxAttempts      *int    `json:"max_attempts"`
}

type rawConfig struct {
	JobTypes map[string]rawJobType `json:"job_types"`
}

// Load reads and checks the file at path.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse checks the file contents. Unknown fields are an error, so a typo
// such as "max_attemps" is caught at startup instead of silently ignored.
func Parse(b []byte) (*Config, error) {
	var raw rawConfig
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if len(raw.JobTypes) == 0 {
		return nil, errors.New("config: job_types is empty; define at least one job type")
	}
	cfg := &Config{JobTypes: make(map[string]JobType, len(raw.JobTypes))}
	for name, r := range raw.JobTypes {
		if name == "" || len(name) > 100 {
			return nil, fmt.Errorf("config: job type name %q must be 1-100 characters", name)
		}
		jt := JobType{
			NeedsApproval:    r.NeedsApproval,
			ApprovalWindow:   DefaultApprovalWindow,
			HandlerTimeLimit: DefaultHandlerTimeLimit,
			MaxAttempts:      DefaultMaxAttempts,
		}
		var err error
		if r.ApprovalWindow != nil {
			if jt.ApprovalWindow, err = positiveDuration(name, "approval_window", *r.ApprovalWindow); err != nil {
				return nil, err
			}
		}
		if r.HandlerTimeLimit != nil {
			if jt.HandlerTimeLimit, err = positiveDuration(name, "handler_time_limit", *r.HandlerTimeLimit); err != nil {
				return nil, err
			}
		}
		if r.MaxAttempts != nil {
			jt.MaxAttempts = *r.MaxAttempts
		}
		if jt.MaxAttempts < MinMaxAttempts || jt.MaxAttempts > MaxMaxAttempts {
			return nil, fmt.Errorf("config: job type %q: max_attempts %d must be between %d and %d",
				name, jt.MaxAttempts, MinMaxAttempts, MaxMaxAttempts)
		}
		cfg.JobTypes[name] = jt
	}
	return cfg, nil
}

func positiveDuration(typ, field, s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("config: job type %q: %s: %w", typ, field, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("config: job type %q: %s must be greater than zero", typ, field)
	}
	return d, nil
}

// DefaultMaxConns is how many database connections one engine may open when DB_MAX_CONNS is not
// set. A fixed number, not the CPU count: a bigger machine would otherwise quietly open more
// connections per engine and several engines would exceed what Postgres allows (ADR 016).
const DefaultMaxConns = 10

// ParseMaxConns reads the DB_MAX_CONNS setting. An empty value is the default. Anything that is
// not a whole number from 1 to 1000 is an error, so a typo stops the engine at startup instead
// of being ignored.
func ParseMaxConns(s string) (int32, error) {
	if s == "" {
		return DefaultMaxConns, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 1000 {
		return 0, fmt.Errorf("DB_MAX_CONNS must be a whole number from 1 to 1000, got %q", s)
	}
	return int32(n), nil
}
