// Package handlers has the demo handlers from spec section 12. They exist for tests,
// load runs and chaos runs; none of them does real work.
//
// Use them with examples/demo-job-types.json, which defines a job type for each.
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"time"

	"github.com/skifast/durable-job-engine/internal/worker"
)

// All returns the handlers by job type name.
func All() map[string]worker.Handler {
	return map[string]worker.Handler{
		"noop":        Noop,
		"sleep":       Sleep,
		"sleep_long":  Sleep, // the same handler; its job type has a longer time limit (used by the lease tests)
		"flaky":       Flaky,
		"always_fail": AlwaysFail,
		"crash":       Crash,
		"stuck":       Stuck,
	}
}

// startedAt is the result of Noop and Sleep: when the handler began. The load tests
// subtract the job's created_at from it to get the start latency (the harness and the
// workers run on one machine, so the clocks agree).
func startedAt() map[string]string {
	return map[string]string{"started_at": time.Now().UTC().Format(time.RFC3339Nano)}
}

// Noop does nothing and succeeds at once. The empty handler of the load test.
func Noop(context.Context, worker.Job) (any, error) { return startedAt(), nil }

// Sleep waits for payload.ms milliseconds (default 100), the 100 ms handler of the load test.
// It stops early if it is cancelled.
func Sleep(ctx context.Context, job worker.Job) (any, error) {
	var p struct{ MS int }
	p.MS = 100
	if len(job.Payload) > 0 {
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			return nil, worker.Permanent(errors.New("payload must be an object like {\"ms\": 100}"))
		}
	}
	res := startedAt()
	select {
	case <-time.After(time.Duration(p.MS) * time.Millisecond):
		return res, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Flaky fails with a retryable error about payload.fail_percent of the time (default 30).
func Flaky(ctx context.Context, job worker.Job) (any, error) {
	var p struct {
		FailPercent int `json:"fail_percent"`
	}
	p.FailPercent = 30
	if len(job.Payload) > 0 {
		json.Unmarshal(job.Payload, &p)
	}
	if rand.Intn(100) < p.FailPercent {
		return nil, worker.Retryable(errors.New("flaky: simulated downstream failure"))
	}
	return map[string]bool{"ok": true}, nil
}

// AlwaysFail fails permanently, so the job goes straight to dead.
func AlwaysFail(context.Context, worker.Job) (any, error) {
	return nil, worker.Permanent(errors.New("always_fail: this handler never succeeds"))
}

// Crash panics. The worker reports an unclassified failure and carries on.
func Crash(context.Context, worker.Job) (any, error) { panic("crash: simulated handler bug") }

// Stuck ignores cancellation and never returns, to show the worker exiting (ADR 014).
// Give its job type a short handler_time_limit.
func Stuck(context.Context, worker.Job) (any, error) { select {} }
