# ADR 012: Implementation language

**Status:** Accepted (2026-10-09)  
**Date:** 2026-10-09  
**Spec sections:** 7 (components), 10 (handler contract)  
**Depends on:** ADR 002 (workers talk to the API), ADR 003 (heartbeats and leases)  

## Context

The engine has four programs to write: the API server, the worker, the reaper, and the operator page's server side. The database layer (`db/`) is already written in SQL and does not depend on this choice.

Three things in the spec put demands on the language:

- The worker runs a handler and must **cancel it** if the handler passes its time limit or the worker loses its lease (spec sections 7 and 10).
- The worker does two things at once: it runs the handler, and it sends a heartbeat every 7.5 s while the handler runs.
- The API server handles many requests at the same time, each with its own short database transaction.

The project is also meant to show backend and distributed-systems skills, so the language is part of what a reader sees.

## Options

### A. Go (chosen)

**For**

- Running two things at once is part of the language (goroutines and channels). A heartbeat loop beside a running handler is a few lines
- The standard `context` package is a common way to cancel work after a deadline, or when the lease is lost. That is exactly the handler-cancellation requirement
- Widely used for infrastructure and distributed systems, which matches the roles this project is aimed at
- Builds to a single program per component, with no runtime to install. Easy to run in tests and chaos runs (start it, kill it)
- The standard library has an HTTP server and a JSON encoder, so the API needs few outside packages

**Against**

- More lines than Python for the same work, and error handling is written out by hand
- Fewer ready-made helpers for things like web forms and database mapping. This project writes its SQL by hand anyway
- A language with its own learning curve, if it is new

### B. Java

**For**

- A direct match for many big-tech backend teams
- Mature libraries and tools for HTTP, database access and testing

**Against**

- More setup and more boilerplate for a small project
- A heavier runtime to start and stop in chaos tests

### C. Python (FastAPI)

**For**

- Fastest to write and easiest to read
- Good test tooling

**Against**

- Cancelling a running handler is harder: Python threads cannot be stopped safely from outside, and async code only cancels at points where the code awaits. A handler stuck in a blocking call would not stop
- A weaker signal for systems and infrastructure roles

## Decision

Use Go for the API server, the worker and the reaper. The operator page's server side is part of the API server.

## Consequences

- The handler contract (spec section 10) can use Go's `context` directly: a handler receives a context that is cancelled when its time limit passes or the lease is lost, and a well-behaved handler stops when it is cancelled.
- A handler that ignores cancellation and keeps running cannot be stopped from outside in Go either. The worker still reports to the engine only while it holds the lease, so a stuck handler cannot overwrite another worker's result (ADR 009). This is a known limit, not something the language fixes.
- Database access, the HTTP framework and the test setup are chosen in the build phase. None of them affects the design. The rule for tests stays the same: run them against a real Postgres, not a mock, because the database enforces the rules (ADR 011).
- The delay function from ADR 004 (attempt number and a random source in, seconds out) is a plain function in Go, so its unit test with a fixed seed is simple.
- The SQL in `db/` is not tied to Go. Another language could replace the programs later without touching the schema or the invariant checks.

## Revisit this decision if

- The build is much slower than expected because of the language, not the design
- A requirement appears that Go handles badly (for example a handler runtime that must run code in another language)
