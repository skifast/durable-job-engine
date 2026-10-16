// Package worker is the program that claims jobs from the engine's API and runs handlers.
//
// The design is in ADR 014 and spec sections 7 and 10. A worker runs one handler at a
// time. While it runs, a second goroutine sends heartbeats. Either of three things
// cancels the handler's context: the time limit, a lost lease, or a shutdown that has
// run out of drain time.
package worker

import (
	"context"
	"encoding/json"
	"errors"
)

// Job is what a handler receives. ID and Attempt let it derive an idempotency key for
// downstream calls: use the ID (the same on every attempt), not the attempt.
type Job struct {
	ID      string
	Type    string
	Payload json.RawMessage
	Attempt int
}

// A Handler does the work for one job type.
//
// Return a nil error for success; the result is stored as JSON (nil is stored as {}),
// and may be at most 64 KB of compact JSON. Wrap an error with Retryable or Permanent
// to classify it. An error you do not wrap is reported as "unclassified": it is
// retried like a retryable one, and recorded under its own kind so a handler that needs
// fixing can be found. A panic is also unclassified.
//
// The handler must stop when ctx is cancelled. It is cancelled when the job type's time
// limit passes, when the worker loses the lease, and when a shutdown has run out of
// drain time. A handler that ignores the cancel makes the worker exit (ADR 014).
type Handler func(ctx context.Context, job Job) (result any, err error)

// classified is an error with a failure kind attached.
type classified struct {
	err  error
	kind string // the report outcome: "retryable" or "permanent"
}

func (c *classified) Error() string { return c.err.Error() }
func (c *classified) Unwrap() error { return c.err }

// Retryable marks err as a failure that a later attempt might get past: a timeout, a
// network error, a 5xx or 429 from a downstream service (spec section 10).
// Retryable(nil) is nil.
func Retryable(err error) error {
	if err == nil {
		return nil
	}
	return &classified{err, "retryable"}
}

// Permanent marks err as a failure that every attempt would hit again: an invalid
// payload, a 404 or 403 from a downstream service. The job goes straight to dead.
// Permanent(nil) is nil.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &classified{err, "permanent"}
}

// kindOf returns the classification set by Retryable or Permanent, or "" if there is none.
func kindOf(err error) string {
	var c *classified
	if errors.As(err, &c) {
		return c.kind
	}
	return ""
}
