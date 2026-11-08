# ADR 017: Public worker package

**Status:** Proposed  
**Date:** 2026-11-08  
**Spec sections:** 7 (components)  
**Depends on:** ADR 002 (workers call the API), ADR 014 (worker and handler design)  
**Changes:** `internal/worker` moves to `worker`; no code inside it changes  

## Context

The engine is meant to run other people's jobs, but the only way to write handlers today is inside this repo. The worker package (the handler type, the API client and the run loop with heartbeats, the time limit and drain on shutdown) sits under `internal/`, and Go refuses to let another module import an `internal/` package.

The first outside user is a job-monitor service in a separate, private repo. It runs the engine as a service and needs its own workers for polling, scoring and notifying. Without this change it would have to copy the worker code or rewrite it against the HTTP API, and lose the ADR 014 behaviour (lease loss, time limit, stuck handler exit) that the tests here prove.

## Decision

Move `internal/worker` to `worker` at the module root, as it is. Imports in `cmd/worker` and `examples/handlers` change; nothing else does.

The package already depends only on the standard library, so making it public exposes no other internal package. Its exported surface is: `Worker`, `Client`, `Item`, `Job`, `Handler`, `Retryable`, `Permanent`, `StatusError`, the `Default*` timing constants and the `Err*` cancellation causes.

Considered and not chosen:

- **A separate client module (`durable-job-engine/sdk`).** Cleaner versioning, but a second `go.mod` to keep in step for one consumer. It can be split out later if the package needs its own release cycle.
- **Leave it internal and have outside repos call the HTTP API directly.** Works, but each one re-implements heartbeats, lease-loss cancellation and the stuck-handler exit, which are the parts most likely to be done wrong.

## Consequences

- The exported API of `worker` is now a promise. Breaking changes need a new major version or a deprecation path, and should get their own ADR.
- Releases get tags (starting with `v0.1.0`) so outside repos can pin a version.
- The client has no `Submit` call yet: the worker only needs claim, heartbeat and report. A caller that submits jobs uses `POST /jobs` directly for now. Adding `Submit` (with the idempotency key) is the likely next change, as its own ADR.

## Verification

`go build ./...` and `go test ./...` pass against Postgres 16, including `worker` (moved) and `examples/handlers`.
