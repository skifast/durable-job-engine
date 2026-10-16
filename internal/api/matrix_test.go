package api_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/skifast/durable-job-engine/internal/store"
	"github.com/skifast/durable-job-engine/testutil"
)

// This file tries every API call against a job in every state, and checks that each call does
// what spec section 6 allows and nothing else. It covers the spec's "never happens" list from
// the outside: a refused call must leave the job exactly as it was (every field, including
// updated_at), and a call that works must produce a state change the spec lists.

// outcome is what one call against one state must do.
type outcome struct {
	status   int
	code     string // the error code, for a refusal
	to       string // the state afterwards; empty means the job must be byte-for-byte unchanged
	replayed bool   // a repeat of a report that was already applied (200, nothing changes)
}

var (
	notOurs    = outcome{status: 409, code: "lease_lost"}
	notWaiting = outcome{status: 409, code: "not_waiting_approval"}
	notDead    = outcome{status: 409, code: "not_dead"}
	nothingDue = outcome{status: 204}
	repeat     = outcome{status: 200, replayed: true}
)

func becomes(state string) outcome { return outcome{status: 200, to: state} }

var states = []string{"queued", "waiting_approval", "running", "succeeded", "dead", "rejected"}

var operations = []string{"heartbeat", "report_success", "report_retryable", "report_permanent", "report_unclassified",
	"approve", "reject", "redrive", "claim"}

// expected is spec section 6 as seen through the API, written from the spec and not from the code.
var expected = map[string]map[string]outcome{
	"queued": {
		"heartbeat": notOurs, "report_success": notOurs, "report_retryable": notOurs, "report_permanent": notOurs,
		"report_unclassified": notOurs, "approve": notWaiting, "reject": notWaiting, "redrive": notDead,
		"claim": becomes("running"), // T6
	},
	"waiting_approval": {
		"heartbeat": notOurs, "report_success": notOurs, "report_retryable": notOurs, "report_permanent": notOurs,
		"report_unclassified": notOurs,
		"approve":             becomes("queued"),   // T3
		"reject":              becomes("rejected"), // T4
		"redrive":             notDead,
		"claim":               nothingDue, // the approval gate: never claimed
	},
	"running": {
		"heartbeat":      becomes("running"),   // T7
		"report_success": becomes("succeeded"), // T8
		// T9: attempts are left (a plain job has 8), so a retryable or unclassified failure requeues
		"report_retryable": becomes("queued"), "report_unclassified": becomes("queued"),
		"report_permanent": becomes("dead"), // T10
		"approve":          notWaiting, "reject": notWaiting, "redrive": notDead, "claim": nothingDue,
	},
	"succeeded": {
		"heartbeat": notOurs, "report_success": repeat, // the same outcome again is a replay
		"report_retryable": notOurs, "report_permanent": notOurs, "report_unclassified": notOurs,
		"approve": notWaiting, "reject": notWaiting, "redrive": notDead, "claim": nothingDue,
	},
	"dead": {
		"heartbeat": notOurs, "report_success": notOurs,
		"report_retryable": repeat, // the attempt ended with a retryable failure, so this is a replay
		"report_permanent": notOurs, "report_unclassified": notOurs,
		"approve": notWaiting, "reject": notWaiting,
		"redrive": becomes("queued"), // T14
		"claim":   nothingDue,
	},
	"rejected": {
		"heartbeat": notOurs, "report_success": notOurs, "report_retryable": notOurs, "report_permanent": notOurs,
		"report_unclassified": notOurs, "approve": notWaiting, "reject": notWaiting, "redrive": notDead,
		"claim": nothingDue,
	},
}

// specPairs are the (from, to) pairs of states in spec section 6 (T3 to T14; T5, the escalation,
// is the reaper's and has no API call).
var specPairs = map[string]bool{
	"waiting_approval>queued": true, "waiting_approval>rejected": true, "queued>running": true,
	"running>running": true, "running>succeeded": true, "running>queued": true, "running>dead": true,
	"dead>queued": true,
}

// forceInto builds a job in the given state through the real calls and returns its id.
func (e *env) forceInto(state string) string {
	e.t.Helper()
	e.t.Cleanup(func() {}) // (nothing to undo: each cell starts with an empty table)
	switch state {
	case "queued":
		return e.mustSubmit("alice", "m-queued", "plain")
	case "waiting_approval":
		return e.mustSubmit("alice", "m-waiting", "gated")
	case "running":
		id := e.mustSubmit("alice", "m-running", "plain")
		e.mustClaim("w1")
		return id
	case "succeeded":
		id := e.mustSubmit("alice", "m-succeeded", "plain")
		e.mustClaim("w1")
		if r := e.report("w1", id, 1, "success", nil); r.status != 200 {
			e.t.Fatalf("setup: %s", r.body)
		}
		return id
	case "dead":
		id := e.mustSubmit("alice", "m-dead", "one_try")
		e.mustClaim("w1")
		if r := e.report("w1", id, 1, "retryable", map[string]any{"message": "boom"}); r.json()["state"] != "dead" {
			e.t.Fatalf("setup: %s", r.body)
		}
		return id
	case "rejected":
		id := e.mustSubmit("alice", "m-rejected", "gated")
		if r := e.call("POST", "/jobs/"+id+"/reject", "op1", nil, map[string]any{"reason": "setup"}); r.status != 200 {
			e.t.Fatalf("setup: %s", r.body)
		}
		return id
	}
	e.t.Fatalf("unknown state %s", state)
	return ""
}

func (e *env) empty() {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), `TRUNCATE job_errors, jobs`); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) apply(op, id string, attempt int) resp {
	switch op {
	case "heartbeat":
		return e.call("POST", "/jobs/"+id+"/heartbeat", "w1", nil, map[string]any{"attempt": attempt})
	case "report_success":
		return e.report("w1", id, attempt, "success", nil)
	case "report_retryable":
		return e.report("w1", id, attempt, "retryable", map[string]any{"message": "boom"})
	case "report_permanent":
		return e.report("w1", id, attempt, "permanent", map[string]any{"message": "bad"})
	case "report_unclassified":
		return e.report("w1", id, attempt, "unclassified", map[string]any{"message": "odd"})
	case "approve":
		return e.call("POST", "/jobs/"+id+"/approve", "op1", nil, nil)
	case "reject":
		return e.call("POST", "/jobs/"+id+"/reject", "op1", nil, map[string]any{"reason": "matrix"})
	case "redrive":
		return e.call("POST", "/jobs/"+id+"/redrive", "op1", nil, map[string]any{"max_attempts": 10})
	case "claim":
		return e.call("POST", "/claim", "w1", nil, nil)
	}
	e.t.Fatalf("unknown operation %s", op)
	return resp{}
}

func TestEveryCallAgainstEveryState(t *testing.T) {
	e := newEnv(t)
	reached := map[string]bool{}
	cells := 0
	for _, state := range states {
		for _, op := range operations {
			want := expected[state][op]
			cells++
			t.Run(state+"/"+op, func(t *testing.T) {
				e.t = t // the helpers report against the subtest
				e.empty()
				id := e.forceInto(state)
				before := e.job(id)
				attempt := 1
				if a := int(before["attempt"].(float64)); a > 1 {
					attempt = a
				}
				if before["state"] != state {
					t.Fatalf("setup: the job is %v, not %s", before["state"], state)
				}

				r := e.apply(op, id, attempt)

				if r.status != want.status {
					t.Fatalf("status %d, want %d: %s", r.status, want.status, r.body)
				}
				if want.code != "" && r.errCode() != want.code {
					t.Errorf("error code %q, want %q", r.errCode(), want.code)
				}
				if want.replayed && r.json()["replayed"] != true {
					t.Errorf("expected a replayed answer: %s", r.body)
				}
				after := e.job(id)
				if want.to == "" {
					// A refusal, a replay, or nothing due: the job must not have changed at all.
					if !reflect.DeepEqual(before, after) {
						t.Errorf("the job changed although nothing was allowed to change:\n before %v\n after  %v", before, after)
					}
				} else {
					if after["state"] != want.to {
						t.Errorf("state after: %v, want %s", after["state"], want.to)
					}
					if op == "claim" && r.json()["id"] != id {
						t.Errorf("claimed a different job: %s", r.body)
					}
					reached[state+">"+want.to] = true
				}
				testutil.CheckInvariants(t, e.pool)
			})
		}
	}
	e.t = t

	// Every state change the API can cause is one the spec lists.
	for pair := range reached {
		if !specPairs[pair] {
			t.Errorf("the API caused %s, which spec section 6 does not allow", pair)
		}
	}
	// ...and every API-driven pair in the spec was exercised (T5, the reaper's, is tested with the reaper).
	for pair := range specPairs {
		if !reached[pair] {
			t.Errorf("no cell of the matrix exercised %s", pair)
		}
	}
	if want := len(states) * len(operations); cells != want {
		t.Errorf("%d cells ran, want %d", cells, want)
	}
}

// A late report and the reaper reach the same job at the same moment. Exactly one wins, and the
// two outcomes are consistent: if the report is accepted the job succeeded and the reaper left it
// alone; if the reaper wins the job is queued with one lease_expired entry and the report is refused.
// (ADR 009: the fence is the attempt, so a report is accepted until the reaper has acted.)
// Both sides call the store directly: going through HTTP makes the report so much slower that
// the reaper almost always wins, and the race would not be a race.
func TestReportRacesReaper(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	reportWon, reaperWon := 0, 0
	for i := 0; i < 200; i++ {
		id := e.mustSubmit("alice", fmt.Sprintf("race-%d", i), "plain")
		e.mustClaim("w1")
		if _, err := e.pool.Exec(ctx, `UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		var reportErr error
		var reaped int
		var reapErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, reportErr = e.store.Report(ctx, store.ReportInput{JobID: id, Worker: "w1", Attempt: 1, Outcome: store.OutcomeSuccess})
		}()
		go func() { defer wg.Done(); <-start; reaped, reapErr = e.store.ReapExpiredLeases(ctx, 100) }()
		close(start)
		wg.Wait()
		if reapErr != nil {
			t.Fatal(reapErr)
		}

		j := e.job(id)
		errs := j["errors"].([]any)
		var lost *store.LeaseLostError
		switch {
		case reportErr == nil:
			reportWon++
			if j["state"] != "succeeded" || len(errs) != 0 || reaped != 0 {
				t.Fatalf("round %d: report accepted but state %v, %d error entries, reaper changed %d", i, j["state"], len(errs), reaped)
			}
		case errors.As(reportErr, &lost):
			reaperWon++
			if j["state"] != "queued" || len(errs) != 1 || reaped != 1 || errs[0].(map[string]any)["kind"] != "lease_expired" {
				t.Fatalf("round %d: report refused but state %v, %d error entries, reaper changed %d", i, j["state"], len(errs), reaped)
			}
		default:
			t.Fatalf("round %d: report failed in an unexpected way: %v", i, reportErr)
		}
		// Clear the requeued job so the next round claims its own.
		e.empty()
	}
	t.Logf("200 races: the report won %d, the reaper won %d", reportWon, reaperWon)
	if reportWon == 0 || reaperWon == 0 {
		t.Errorf("only one side ever won (report %d, reaper %d): the test is not exercising the race", reportWon, reaperWon)
	}
}
