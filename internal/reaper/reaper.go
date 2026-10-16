// Package reaper runs the background pass that takes back jobs whose lease has
// run out (T12, T13) and flags approvals that passed their deadline (T5).
//
// It runs inside the engine process (ADR 012). Running a second copy, in another
// process or on another machine, is safe: the store re-checks each job under a row
// lock, so a job is requeued or escalated only once (spec section 4).
package reaper

import (
	"context"
	"log/slog"
	"time"

	"github.com/skifast/durable-job-engine/internal/store"
)

const (
	// DefaultInterval is how often the reaper looks (spec section 7). It adds up to
	// this long to crash recovery, on top of the 30 s lease.
	DefaultInterval = 5 * time.Second
	// DefaultBatch is the most jobs one tick handles of each kind. It keeps a tick
	// short after a big outage; the rest wait for the next tick.
	DefaultBatch = 100
)

// Reaper holds the settings for the loop. Zero values mean the defaults.
type Reaper struct {
	Store    *store.Store
	Log      *slog.Logger
	Interval time.Duration
	Batch    int
}

// Run ticks until ctx is cancelled. It starts with a tick right away, so jobs
// orphaned while the engine was down are taken back at startup, not 5 s later.
func (r *Reaper) Run(ctx context.Context) {
	interval := r.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		r.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick does one pass. An error is logged and the loop carries on: the database
// may be restarting, and the next tick will try again.
func (r *Reaper) Tick(ctx context.Context) {
	batch := r.Batch
	if batch <= 0 {
		batch = DefaultBatch
	}
	n, err := r.Store.ReapExpiredLeases(ctx, batch)
	if err != nil && ctx.Err() == nil {
		r.Log.Error("reaper: expired leases", "error", err)
	}
	if n > 0 {
		r.Log.Info("reaper: took back jobs with expired leases", "count", n)
	}
	n, err = r.Store.EscalateOverdueApprovals(ctx, batch)
	if err != nil && ctx.Err() == nil {
		r.Log.Error("reaper: overdue approvals", "error", err)
	}
	if n > 0 {
		r.Log.Info("reaper: escalated overdue approvals", "count", n)
	}
}
