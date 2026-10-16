# ADR 014: Worker and handler design

**Status:** Accepted (2026-10-16)  
**Date:** 2026-10-16  
**Spec sections:** 7 (worker), 10 (handler contract), 11 (failure modes)  
**Depends on:** ADR 003 (lease and heartbeat), ADR 009 (fencing token), ADR 012 (Go)  
**Changes:** the work item from `/claim` gains one field, `handler_time_limit_ms` (docs/api.md)  

## Context

Spec section 10 says what a handler must do but not what it looks like in Go, and section 7 says the worker cancels a handler that passes its time limit, without saying how a worker finds out the limit or what happens if the handler ignores the cancel. Five choices were open. Terms: a **context** is Go's standard cancel signal, which a handler is expected to watch and stop on. A **goroutine** is Go's lightweight thread. **SIGTERM** is the signal a deploy tool sends to ask a process to shut down.

## Decisions

### 1. Handler shape: return `(result, error)`, classify by wrapping the error

A handler is `func(ctx context.Context, job Job) (any, error)`. A nil error is success, and the result is stored as JSON. `Retryable(err)` and `Permanent(err)` wrap an error to classify it. An error with no wrapping is reported as `unclassified`, with its Go type and message, as spec section 10 requires. A panic is also reported as `unclassified`.

Considered and not chosen: a handler that returns an outcome value (`Success(...)`, `Retryable(...)`). It is explicit, but it does not look like ordinary Go, and the `unclassified` safety net would have to be built by hand.

### 2. The time limit travels in the work item

The engine's job-types file is the only place the handler time limit is set. `/claim` now returns it as `handler_time_limit_ms` (whole milliseconds). If a job's type is no longer in the file (the file changed after the job was submitted), the engine sends the 60 s default.

Considered and not chosen: the worker reads the same file. That makes two files that must agree, and a mismatch would use the wrong limit without any error.

### 3. A handler that ignores cancellation: the worker exits

When the worker cancels a handler (time limit, lease lost, or shutdown) and the handler does not return within 5 s (the **stuck grace**), the worker stops and exits with an error. Go cannot stop a goroutine from outside, so this is the only way to be sure nothing is still running when the worker moves on. It needs something to restart workers (a Docker restart policy, systemd, Kubernetes). The engine already treats a dead worker as normal.

Considered and not chosen: waiting for the handler (the worker stalls, its lease runs out, another worker reruns the job while the stuck handler may still be running), and abandoning the stuck handler and moving on (stuck handlers pile up and "one handler at a time" stops being true).

On a time limit, the worker reports a `retryable` failure ("handler exceeded its time limit of 60s") at once, then cancels, then waits for the grace period. If the lease was lost, it does not report, because the report would be refused.

### 4. Shutdown: drain, then cancel

On SIGTERM the worker stops claiming. A handler that is running gets up to 30 s (the **drain window**, adjustable) to finish, and its result is reported as usual. If it is still running after that, the worker cancels it, reports `retryable` ("worker shutting down") and exits. An interrupted job always uses up one attempt, because there is no call to give a job back without penalty. Such a call would change the API and is not in this version.

Considered and not chosen: cancelling at once (nearly finished work is thrown away) and exiting without reporting (the job waits for lease expiry plus the reaper plus the first retry delay, about 37 s, and the error history says nothing).

### 5. A job type with no handler: report `retryable`

Any worker can claim any job (no type filter, docs/api.md decision 5), so a worker can claim a type it has no handler for, mostly during a rolling deploy. The worker reports `retryable` with the message "no handler for type X". Backoff gives a worker that has the handler a chance to take it. It costs one attempt each time.

Considered and not chosen: `permanent`, which sends the job to dead because of a deployment mistake.

## Defaults set with these decisions

| Setting | Value |
|---|---|
| Stuck grace | 5 s |
| Drain window | 30 s |
| HTTP timeout per request | 10 s |
| Report retry | retry with backoff for up to 30 s (one lease length), then log and give up; the reaper takes over. The replay rule (docs/api.md) makes a repeated report safe |
| Rejected key (401 or 403 on claim) | the worker stops with an error instead of polling forever |
| A result over 64 KB (413 from `/report`) | the worker reports a `permanent` failure, as docs/api.md says |
| Heartbeat during the final report | stopped before the report is sent |

## Consequences

- A handler author writes ordinary Go and classifies only the errors they understand.
- A stuck handler costs a worker restart, and the job is reported as a retryable timeout first.
- Deploys cost an attempt for each job that is still running after 30 s.
- A handler that returns after its time limit has passed has its result thrown away, because the timeout is already reported. Handlers must be safe to run twice, as the spec already says.
- Honest limit: between the time-limit report and the end of the grace period, the old handler may still be doing work while another worker runs the retry. Delivery is at least once, so this is the same duplicate-work case as a lease expiry.
