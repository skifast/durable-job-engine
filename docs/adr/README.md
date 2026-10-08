# Architecture decision records

An architecture decision record (ADR) is a short document about one design decision: the context, the options considered and their tradeoffs, the decision, and its consequences. They explain why the system is built the way it is, and when to revisit each choice.

**Statuses:** *Proposed* means written and under review. *Accepted* means reviewed and agreed.

| # | Decision | Status | Chosen |
|---|---|---|---|
| [001](001-postgres-as-queue-store.md) | Postgres as the queue store | Accepted | Postgres, with jobs claimed using `SELECT ... FOR UPDATE SKIP LOCKED` |
| [002](002-workers-call-api.md) | Workers talk to the API, not to Postgres | Proposed | Workers call `/claim`, `/heartbeat` and `/report` |
| [003](003-lease-and-heartbeat.md) | Lease length and heartbeat interval | Proposed | 30 s lease, 7.5 s heartbeat |
| [004](004-retry-backoff-and-jitter.md) | Retry curve and jitter | Proposed | 8 attempts, 2 s base doubling, 60 s cap, 0-10% jitter |
| [005](005-duplicate-key-handling.md) | Duplicate-key handling, scope and retention | Proposed | Per-client keys kept forever; 200, 422 and 409 for the three duplicate cases |
| [006](006-approval-placement-and-timeout.md) | Approval placement and timeout | Accepted | Gate before the handler; escalate on timeout |
| [007](007-lease-expiry-requeue.md) | What happens when a lease expires | Proposed | Requeue with backoff, same as a failure |
| [008](008-durability.md) | Durability level | Proposed | Single node, default `synchronous_commit` |
| [009](009-fencing-token-and-redrive.md) | Fencing token and redrive | Accepted | The attempt number is the token; redrive never resets it |
| [010](010-ui-scope.md) | UI scope | Proposed | Operator page on the public API, no login |

Related: [spec](../spec.md) · [metrics and settings](../metrics-and-settings.md)
