package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Role is what an API key may do (ADR 013).
type Role string

const (
	RoleClient   Role = "client"
	RoleWorker   Role = "worker"
	RoleOperator Role = "operator"
)

// Key is the caller a request came from, found by looking up the key's hash.
type Key struct {
	Name string
	Role Role
}

// Time is a timestamp that writes itself as RFC 3339 in UTC with milliseconds
// (2026-10-09T20:15:30.123Z), the format in docs/api.md.
type Time time.Time

func (t Time) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Time(t).UTC().Format("2006-01-02T15:04:05.000Z") + `"`), nil
}

// Scan lets pgx read a timestamptz column straight into a Time or *Time.
func (t *Time) Scan(src any) error {
	v, ok := src.(time.Time)
	if !ok {
		return fmt.Errorf("cannot read %T as a timestamp", src)
	}
	*t = Time(v)
	return nil
}

// Std returns the value as a time.Time.
func (t Time) Std() time.Time { return time.Time(t) }

// Job is the full representation of a job (docs/api.md, "Full job").
// A field with no value is null in the JSON, never left out.
type Job struct {
	ID               string          `json:"id"`
	Type             string          `json:"type"`
	ClientID         string          `json:"client_id"`
	IdempotencyKey   string          `json:"idempotency_key"`
	State            string          `json:"state"`
	Attempt          int             `json:"attempt"`
	MaxAttempts      int             `json:"max_attempts"`
	RunAt            Time            `json:"run_at"`
	LeaseOwner       *string         `json:"lease_owner"`
	LeaseExpiresAt   *Time           `json:"lease_expires_at"`
	ApprovalDeadline *Time           `json:"approval_deadline"`
	EscalatedAt      *Time           `json:"escalated_at"`
	DecidedBy        *string         `json:"decided_by"`
	DecidedAt        *Time           `json:"decided_at"`
	DecisionReason   *string         `json:"decision_reason"`
	RedrivenBy       *string         `json:"redriven_by"`
	RedrivenAt       *Time           `json:"redriven_at"`
	Payload          json.RawMessage `json:"payload"`
	Result           json.RawMessage `json:"result"` // nil is written as null
	Errors           []ErrorEntry    `json:"errors"`
	CreatedAt        Time            `json:"created_at"`
	UpdatedAt        Time            `json:"updated_at"`
	FinishedAt       *Time           `json:"finished_at"`
}

// ErrorEntry is one attempt that ended without succeeding.
type ErrorEntry struct {
	Attempt    int    `json:"attempt"`
	Kind       string `json:"kind"`
	Message    string `json:"message"`
	OccurredAt Time   `json:"occurred_at"`
}

// WorkItem is what a worker gets from a claim: only what a handler needs.
// Attempt is the fencing token.
type WorkItem struct {
	ID             string          `json:"id"`
	Type           string          `json:"type"`
	Payload        json.RawMessage `json:"payload"`
	Attempt        int             `json:"attempt"`
	LeaseExpiresAt Time            `json:"lease_expires_at"`
}

// Errors the store returns for situations the API answers with a specific status.
var (
	ErrNotFound   = errors.New("job not found")
	ErrKeyReused  = errors.New("idempotency key used with a different request") // 422
	ErrInProgress = errors.New("same idempotency key is still being saved")     // 409
	ErrBadContent = errors.New("content Postgres cannot store")                 // 400, for example \u0000 in a string
)

// LeaseLostError means the fence did not match (docs/api.md, 409 lease_lost).
// State is where the job is now.
type LeaseLostError struct{ State string }

func (e *LeaseLostError) Error() string { return "lease lost; job is " + e.State }

// NotWaitingError means an approve or reject hit a job that is not waiting_approval
// (docs/api.md, 409 not_waiting_approval). DecidedBy is set when someone already decided.
type NotWaitingError struct {
	State     string
	DecidedBy *string
}

func (e *NotWaitingError) Error() string { return "job is " + e.State + ", not waiting_approval" }

// NotDeadError means a redrive hit a job that is not dead (409 not_dead).
type NotDeadError struct{ State string }

func (e *NotDeadError) Error() string { return "job is " + e.State + ", not dead" }

// MaxAttemptsTooLowError means a redrive asked for max_attempts that is not above the
// job's attempt count (400 max_attempts_too_low).
type MaxAttemptsTooLowError struct{ Attempt int }

func (e *MaxAttemptsTooLowError) Error() string { return "max_attempts must be greater than attempt" }

// ErrBadCursor means the cursor of a list call is not one this engine made (400).
var ErrBadCursor = errors.New("bad cursor")

// Summary is a job in a list: no payload, result or full error history (docs/api.md).
type Summary struct {
	ID               string     `json:"id"`
	Type             string     `json:"type"`
	ClientID         string     `json:"client_id"`
	State            string     `json:"state"`
	Attempt          int        `json:"attempt"`
	MaxAttempts      int        `json:"max_attempts"`
	RunAt            Time       `json:"run_at"`
	ApprovalDeadline *Time      `json:"approval_deadline"`
	EscalatedAt      *Time      `json:"escalated_at"`
	CreatedAt        Time       `json:"created_at"`
	FinishedAt       *Time      `json:"finished_at"`
	LastError        *LastError `json:"last_error"`
}

// LastError is the newest entry of a job's error history.
type LastError struct {
	Attempt int    `json:"attempt"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}
