package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"
)

// Defaults for the timing settings (docs/metrics-and-settings.md, ADR 003 and ADR 014).
const (
	DefaultPollInterval      = 100 * time.Millisecond
	DefaultHeartbeatInterval = 7500 * time.Millisecond
	DefaultLeaseLength       = 30 * time.Second
	DefaultDrainWindow       = 30 * time.Second
	DefaultStuckGrace        = 5 * time.Second
	DefaultReportRetryFor    = 30 * time.Second
	DefaultHandlerTimeLimit  = 60 * time.Second
	DefaultErrorPause        = time.Second
)

// ErrHandlerStuck is returned by Run when a cancelled handler did not return within the
// stuck grace. Go cannot stop a goroutine from outside, so the worker stops instead and
// something has to restart it (ADR 014).
var ErrHandlerStuck = errors.New("a cancelled handler did not stop; the worker must exit")

// Why a handler's context was cancelled. Handlers can read it with context.Cause(ctx).
var (
	ErrTimeLimit = errors.New("handler time limit passed")
	ErrLeaseGone = errors.New("lease lost")
	ErrShutdown  = errors.New("worker shutting down")
)

// Worker claims jobs and runs their handlers, one at a time. Zero timing fields take
// the defaults above; tests set them small.
type Worker struct {
	Client   *Client
	Handlers map[string]Handler // by job type
	Log      *slog.Logger

	PollInterval      time.Duration // wait after /claim finds nothing
	HeartbeatInterval time.Duration
	LeaseLength       time.Duration // how long without a successful heartbeat before the handler is cancelled
	DrainWindow       time.Duration // how long a running handler may finish after shutdown starts
	StuckGrace        time.Duration // how long a cancelled handler has to return
	ReportRetryFor    time.Duration // how long a report is retried
	ErrorPause        time.Duration // wait after /claim fails
}

func (w *Worker) setDefaults() {
	set := func(p *time.Duration, d time.Duration) {
		if *p <= 0 {
			*p = d
		}
	}
	set(&w.PollInterval, DefaultPollInterval)
	set(&w.HeartbeatInterval, DefaultHeartbeatInterval)
	set(&w.LeaseLength, DefaultLeaseLength)
	set(&w.DrainWindow, DefaultDrainWindow)
	set(&w.StuckGrace, DefaultStuckGrace)
	set(&w.ReportRetryFor, DefaultReportRetryFor)
	set(&w.ErrorPause, DefaultErrorPause)
	if w.Log == nil {
		w.Log = slog.Default()
	}
}

// Run claims and runs jobs until ctx is cancelled (a shutdown), then returns nil. It
// returns an error if the engine rejects the worker's key or a handler is stuck.
//
// A shutdown does not cancel a running handler at once: it gets the drain window to finish.
func (w *Worker) Run(ctx context.Context) error {
	w.setDefaults()
	for ctx.Err() == nil {
		sent := time.Now() // the lease starts at the engine, a little after this
		// The claim is not cancelled by a shutdown. If it were, the engine might hand us a job
		// whose answer we never read, and that job would sit until its lease ran out.
		item, err := w.Client.Claim(context.WithoutCancel(ctx))
		switch {
		case errors.Is(err, ErrRejectedKey):
			return err
		case err != nil:
			w.Log.Warn("claim failed", "error", err)
			sleep(ctx, w.ErrorPause)
			continue
		case item == nil:
			sleep(ctx, w.PollInterval)
			continue
		}
		if err := w.runJob(ctx, item, sent); err != nil {
			return err
		}
	}
	return nil
}

// sleep waits for d or until ctx is done.
func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// handlerResult is what the handler goroutine sends back.
type handlerResult struct {
	value any
	err   error
	panic any // set if the handler panicked
}

// runJob runs one claimed job. It returns ErrHandlerStuck or nil.
func (w *Worker) runJob(shutdown context.Context, item *Item, claimSent time.Time) error {
	log := w.Log.With("job_id", item.ID, "attempt", item.Attempt, "type", item.Type)

	h, ok := w.Handlers[item.Type]
	if !ok {
		// Another worker may have the handler (ADR 014, decision 5).
		log.Warn("no handler for this job type")
		w.report(log, item, "retryable", nil, "no handler for type "+item.Type)
		return nil
	}
	limit := time.Duration(item.HandlerTimeLimitMS) * time.Millisecond
	if limit <= 0 {
		limit = DefaultHandlerTimeLimit
	}

	// The handler's context is not derived from the shutdown context: a shutdown starts
	// a drain window, it does not cancel the handler.
	hctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	done := make(chan handlerResult, 1) // room for one, so a late handler can finish and be forgotten
	go func() {
		var r handlerResult
		defer func() {
			if p := recover(); p != nil {
				log.Error("handler panicked", "panic", p, "stack", string(debug.Stack()))
				r = handlerResult{panic: p}
			}
			done <- r
		}()
		r.value, r.err = h(hctx, Job{ID: item.ID, Type: item.Type, Payload: item.Payload, Attempt: item.Attempt})
	}()

	// Heartbeats run in their own goroutine, so a slow heartbeat never delays noticing
	// that the handler finished.
	hbCtx, stopHB := context.WithCancel(context.Background())
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		w.heartbeats(hbCtx, log, item, claimSent, cancel)
	}()
	stopHeartbeats := func() { stopHB(); <-hbDone }
	defer stopHeartbeats()

	limitTimer := time.NewTimer(limit)
	defer limitTimer.Stop()
	shutdownC := shutdown.Done()
	var drainC <-chan time.Time

	for {
		select {
		case r := <-done:
			stopHeartbeats()
			w.reportHandlerResult(log, item, r)
			return nil

		case <-limitTimer.C:
			// Report first (spec section 6: a handler past its limit is a retryable failure),
			// then cancel and give the handler a moment to stop.
			log.Warn("handler passed its time limit", "limit", limit)
			cancel(ErrTimeLimit)
			stopHeartbeats()
			w.report(log, item, "retryable", nil, fmt.Sprintf("handler exceeded its time limit of %s", limit))
			return w.waitForStop(log, done)

		case <-hctx.Done():
			// Only the heartbeat goroutine cancels hctx before we do: the lease is gone, so any
			// report would be refused. Do not report.
			log.Warn("lease lost; handler cancelled", "cause", context.Cause(hctx))
			stopHeartbeats()
			return w.waitForStop(log, done)

		case <-shutdownC:
			shutdownC = nil
			log.Info("shutting down; letting the handler finish", "drain_window", w.DrainWindow)
			drain := time.NewTimer(w.DrainWindow)
			defer drain.Stop()
			drainC = drain.C

		case <-drainC:
			log.Warn("drain window ended; cancelling the handler")
			cancel(ErrShutdown)
			stopHeartbeats()
			w.report(log, item, "retryable", nil, "worker shutting down")
			return w.waitForStop(log, done)
		}
	}
}

// waitForStop gives a cancelled handler the stuck grace to return.
func (w *Worker) waitForStop(log *slog.Logger, done <-chan handlerResult) error {
	t := time.NewTimer(w.StuckGrace)
	defer t.Stop()
	select {
	case <-done: // the result is dropped: the outcome was decided before the cancel
		return nil
	case <-t.C:
		log.Error("handler ignored its cancellation; the worker will exit", "grace", w.StuckGrace)
		return ErrHandlerStuck
	}
}

// heartbeats renews the lease every HeartbeatInterval until ctx ends. It cancels the
// handler if the engine says the lease is gone, or if no heartbeat has got through for a
// whole lease length. That time is measured on this machine's monotonic clock from when
// the last successful request was SENT, so the worker stops no later than the database's
// expiry (ADR 003).
func (w *Worker) heartbeats(ctx context.Context, log *slog.Logger, item *Item, claimSent time.Time, cancel context.CancelCauseFunc) {
	lastOK := claimSent
	tick := time.NewTicker(w.HeartbeatInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		deadline := lastOK.Add(w.LeaseLength)
		// A call that outlives the deadline is cut off there, so we notice on time.
		callCtx, callCancel := context.WithDeadline(ctx, deadline)
		sent := time.Now()
		err := w.Client.Heartbeat(callCtx, item.ID, item.Attempt)
		callCancel()
		switch {
		case err == nil:
			lastOK = sent
		case ctx.Err() != nil:
			return
		case errors.Is(err, ErrLeaseLost):
			cancel(ErrLeaseGone)
			return
		default:
			log.Warn("heartbeat failed", "error", err)
		}
		if !time.Now().Before(lastOK.Add(w.LeaseLength)) {
			log.Warn("no heartbeat got through for a whole lease length")
			cancel(ErrLeaseGone)
			return
		}
	}
}

// reportHandlerResult turns what the handler returned into a report.
func (w *Worker) reportHandlerResult(log *slog.Logger, item *Item, r handlerResult) {
	switch {
	case r.panic != nil:
		w.report(log, item, "unclassified", nil, fmt.Sprintf("panic: %v", r.panic))
	case r.err == nil:
		var raw json.RawMessage
		if r.value != nil {
			b, err := json.Marshal(r.value)
			if err != nil {
				w.report(log, item, "permanent", nil, "handler result cannot be encoded as JSON: "+err.Error())
				return
			}
			raw = b
		}
		w.report(log, item, "success", raw, "")
	default:
		kind := kindOf(r.err)
		msg := r.err.Error()
		if kind == "" {
			// spec section 10: the error's type and message
			kind, msg = "unclassified", fmt.Sprintf("%T: %s", r.err, r.err)
		}
		w.report(log, item, kind, nil, msg)
	}
}

// reportBackoff is the first wait between report retries; it doubles up to reportBackoffMax.
const (
	reportBackoff    = 200 * time.Millisecond
	reportBackoffMax = 2 * time.Second
)

// report sends an outcome, retrying while the engine or network is down, for at most
// ReportRetryFor. A repeated report is safe: the engine answers a repeat with replayed:true
// (docs/api.md). It is not tied to the shutdown context, so a shutdown still reports.
func (w *Worker) report(log *slog.Logger, item *Item, outcome string, result json.RawMessage, message string) {
	ctx, cancel := context.WithTimeout(context.Background(), w.ReportRetryFor)
	defer cancel()
	wait := reportBackoff
	for {
		err := w.Client.Report(ctx, item.ID, item.Attempt, outcome, result, message)
		var se *StatusError
		switch {
		case err == nil:
			log.Info("reported", "outcome", outcome)
			return
		case errors.Is(err, ErrLeaseLost):
			log.Warn("report refused: the job is no longer ours", "outcome", outcome)
			return
		case errors.Is(err, ErrResultTooLarge) && outcome == "success":
			// A handler that returns too much has a bug a retry will not fix (docs/api.md, decision 2).
			outcome, result, message = "permanent", nil, "handler result is over 64 KB of compact JSON"
			continue
		case errors.Is(err, ErrRejectedKey):
			log.Error("report refused: the engine rejected this worker's key")
			return
		case errors.As(err, &se) && !se.retryable():
			log.Error("report refused as invalid; not retrying", "error", err)
			return
		}
		log.Warn("report failed; will retry", "error", err, "wait", wait)
		select {
		case <-ctx.Done():
			log.Error("gave up reporting; the reaper will take the job back when its lease ends", "outcome", outcome)
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, reportBackoffMax)
	}
}
