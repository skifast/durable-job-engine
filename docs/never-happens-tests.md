# Tests for the "never happens" list

Spec section 6 lists seven things that must never happen, and section 12 asks for a test of each. This page says which tests cover each one, how they were checked, and what they do not cover.

There are three layers, and the first two are separate on purpose. The database refuses illegal changes by itself (ADR 011), so a bug in the API or the reaper cannot corrupt a job. The API and the reaper are also tested to not even try.

| Layer | Tests | How to run |
|---|---|---|
| Database | `db/tests/test_schema.sql`, parts 2, 5, 7 and 8b | `psql -v ON_ERROR_STOP=1 -f db/tests/test_schema.sql <scratch database>` (the schema and `invariants.sql` loaded first) |
| API | `internal/api`: `TestEveryCallAgainstEveryState` (in `matrix_test.go`), `TestHeartbeat`, `TestReportFencing`, `TestConcurrentReports`, `TestReportRacesReaper`, and the operator tests | `go test ./internal/api` |
| Reaper | `internal/reaper`: `TestReaperNeverRejectsAnApproval`, `TestEscalationSkipsJobsThatAreNotWaiting`, `TestEscalatesOverdueApprovalOnce` | `go test ./internal/reaper` |

## The seven cases

| # | Must never happen | Database | API | Reaper |
|---|---|---|---|---|
| 1 | `queued → succeeded` (skipping running) | Part 7 names it. Part 8b tries it among all 36 pairs of states | Matrix: `queued/report_success` is refused with 409 and the job is unchanged. `TestReportFencing`: a late success for a job the reaper put back in the queue is refused | |
| 2 | `waiting_approval → running` (skipping the approval gate) | Part 5 names it, and checks that a waiting job is never claimed. Part 8b tries it | Matrix: `waiting_approval/claim` answers 204, and a heartbeat or report on a waiting job is refused with the job unchanged. `TestApprove`: nothing can be claimed while a job waits | |
| 3 | Anything out of `succeeded` or `rejected` | Part 8b tries all 12 pairs that start in one of them (`succeeded → X` and `rejected → X` for each of the six states), and parts 5 and 7 name some | Matrix: every call against a `succeeded` job and against a `rejected` job (18 cells) leaves it byte-for-byte unchanged, or answers a repeat of the same report with `replayed: true`. `TestSecondDecisionIsRefused` | |
| 4 | `running → waiting_approval` | Part 7 names it. Part 8b tries it | No call can do it. Matrix: `running/approve` and `running/reject` are refused, job unchanged | |
| 5 | `waiting_approval → rejected` by a timeout (only an explicit reject call does this) | Part 5: an escalation sets `escalated_at` and the state stays `waiting_approval`. (The database cannot tell a timeout from an explicit reject, since T4 is legal for a person, so the guarantee is in the reaper's code, next column.) | `TestReject`, `TestSecondDecisionIsRefused`: a reject is by an operator key, and the reason is recorded | `TestReaperNeverRejectsAnApproval`: five passes after the deadline, and the job stays `waiting_approval`, with no decided_by, decision time, reason or finished time, and `escalated_at` set once and never moved |
| 6 | Escalating a job a second time, or a job that is not `waiting_approval` | Part 5: a second escalation is guarded and a forced one is refused. Part 8b: escalating an approved (queued), a running, and a rejected job is refused, and nothing is marked | | `TestEscalatesOverdueApprovalOnce`: a second pass changes nothing. `TestThreeReapersEscalatingTogetherEscalateEachJobOnce`: three reapers released at the same moment over 500 overdue approvals escalate each exactly once and `escalated_at` never moves (ADR 016). `TestEscalationSkipsJobsThatAreNotWaiting`: an overdue job that was approved, claimed, or rejected is skipped, while a waiting one beside them is escalated |
| 7 | Any change made by a worker whose `lease_owner` and `attempt` do not match | Part 2 (the fenced updates). Part 7: a heartbeat cannot change the lease owner | `TestHeartbeat` and `TestReportFencing`: wrong worker, wrong attempt, a stale attempt after a newer one, and after the reaper took the job back, each refused with 409 and nothing changed. `TestConcurrentReports`. `TestReportRacesReaper`: 200 rounds, per run, of a late report and the reaper hitting one job at the same moment. About half go each way and every round is consistent | `TestRenewedLeaseIsNotReaped`, `TestReapTwiceRequeuesOnce` (the reaper side of the same fence), `TestThreeReapersFiringTogetherRequeueEachJobOnce` (three reapers released at the same moment over 1,000 expired leases requeue each exactly once) |

## What the matrix is

`TestEveryCallAgainstEveryState` builds a job in each of the six states with the real calls, then tries each of nine calls against it (heartbeat, four kinds of report, approve, reject, redrive, claim): 54 cases. The expected result of each is written in the test from spec section 6, not copied from the code. A refusal must leave the job exactly as it was, down to `updated_at`. A call that succeeds must give a state change that the spec lists. It also checks that every spec transition a client can cause (all but T5, the reaper's) was exercised, and that the invariant views are clean after every case.

Part 8b of the database test does the same for state changes at the database level. The spec allows 9 of the 36 pairs of states. Part 8b writes those 9 down from the spec and tries the other 27, each of which must be refused.

## How the tests were checked

A test that has never failed proves little, so each new test was run against code made wrong on purpose, and the code was then restored:

- The database matrix: a copy of the schema that allows `dead → succeeded` (a pair no older test names). Part 7 still passed, and part 8b failed on exactly that pair.
- The API matrix: `Approve` made to ignore the job's state. The matrix failed in five cells (`approve` against queued, running, succeeded, dead and rejected jobs).
- The reaper: the escalation made to ignore the job's state. `TestEscalationSkipsJobsThatAreNotWaiting` failed. The database refused the escalation too ("only the escalation (T5) can set escalated_at"), so a reaper bug would be stopped by the second layer as well.

## Things found while writing them

- **Part 9 of the database test weakens the table for everything after it.** It turns off every trigger on `jobs` and drops every CHECK and UNIQUE constraint to plant violations, and relies on the final rollback to undo it. A new part placed after it would pass for the wrong reason. That is how a refusal known to work in part 7 looked accepted in a first version of part 8b. The new part sits before part 9, and part 9 now carries a warning comment.
- **The report–reaper race is mostly won by the reaper when the report goes through HTTP**, because the HTTP round trip is slower (38 of 40 in a first version). The race test therefore calls the store directly on both sides, which splits it about evenly.

## Not covered

- **The reaper's escalation in the database tests is a copy of its SQL**, not the real code (the real code is tested in `internal/reaper`).
- **The matrix uses the right worker and attempt** to isolate the state dimension. Wrong worker and wrong attempt are `TestReportFencing`'s job.
