package api_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
)

// wantIDs compares two lists of job ids.
func wantIDs(t *testing.T, what string, got, wantv []string) {
	t.Helper()
	if !slices.Equal(got, wantv) {
		t.Errorf("%s: got %v, want %v", what, got, wantv)
	}
}

// submitGated submits a job of the type that needs approval and returns its id.
func (e *env) submitGated(key string) string { return e.mustSubmit("alice", key, "gated") }

// makeOverdue moves a waiting job's approval deadline into the past and lets the real
// reaper escalate it. approval_deadline never changes in real use (the trigger refuses
// it), so the test turns triggers off for this one change, as makeDue does.
func (e *env) makeOverdue(id string) {
	e.t.Helper()
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL session_replication_role = replica"); err != nil {
		e.t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET approval_deadline = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		e.t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		e.t.Fatal(err)
	}
	if n, err := e.store.EscalateOverdueApprovals(ctx, 100); err != nil || n != 1 {
		e.t.Fatalf("escalated %d, err %v", n, err)
	}
}

func (e *env) makeDead(key string) string {
	e.t.Helper()
	id := e.mustSubmit("alice", key, "one_try")
	e.mustClaim("w1")
	if r := e.report("w1", id, 1, "retryable", map[string]any{"message": "boom"}); r.json()["state"] != "dead" {
		e.t.Fatalf("job did not die: %s", r.body)
	}
	return id
}

// ---------------------------------------------------------------------------
// Approve and reject
// ---------------------------------------------------------------------------

func TestApprove(t *testing.T) {
	e := newEnv(t)
	id := e.submitGated("a1")
	want(t, "nothing to claim while waiting", e.call("POST", "/claim", "w1", nil, nil).status, 204)

	r := e.call("POST", "/jobs/"+id+"/approve", "op1", nil, map[string]any{"reason": "looks fine"})
	want(t, "status", r.status, 200)
	j := r.json()
	want(t, "state", j["state"], "queued")
	want(t, "decided_by is the key's name", j["decided_by"], "op1")
	want(t, "reason", j["decision_reason"], "looks fine")
	if j["decided_at"] == nil {
		t.Error("decided_at not set")
	}
	want(t, "now claimable", e.mustClaim("w1")["id"], id)

	// The body is optional.
	id2 := e.submitGated("a2")
	r = e.call("POST", "/jobs/"+id2+"/approve", "op1", nil, nil)
	want(t, "no body", r.status, 200)
	want(t, "no reason", r.json()["decision_reason"], nil)
}

func TestApproveAfterEscalation(t *testing.T) {
	e := newEnv(t)
	id := e.submitGated("esc")
	e.makeOverdue(id)
	want(t, "still waiting", e.job(id)["state"], "waiting_approval")
	r := e.call("POST", "/jobs/"+id+"/approve", "op1", nil, nil)
	want(t, "status", r.status, 200)
	want(t, "state", r.json()["state"], "queued")
	if r.json()["escalated_at"] == nil {
		t.Error("escalated_at should stay after the approval")
	}
}

func TestReject(t *testing.T) {
	e := newEnv(t)
	id := e.submitGated("r1")
	for name, body := range map[string]any{"no body": nil, "no reason": map[string]any{}, "blank reason": map[string]any{"reason": "  "}} {
		r := e.call("POST", "/jobs/"+id+"/reject", "op1", nil, body)
		if r.status != 400 || r.errCode() != "invalid_request" {
			t.Errorf("%s: got %d %s", name, r.status, r.body)
		}
	}
	long := make([]rune, 1001)
	for i := range long {
		long[i] = 'x'
	}
	want(t, "reason over 1000", e.call("POST", "/jobs/"+id+"/reject", "op1", nil, map[string]any{"reason": string(long)}).status, 400)
	want(t, "unchanged after refusals", e.job(id)["state"], "waiting_approval")

	r := e.call("POST", "/jobs/"+id+"/reject", "op1", nil, map[string]any{"reason": "duplicate"})
	want(t, "status", r.status, 200)
	j := r.json()
	want(t, "state", j["state"], "rejected")
	want(t, "decided_by", j["decided_by"], "op1")
	want(t, "reason", j["decision_reason"], "duplicate")
	if j["finished_at"] == nil {
		t.Error("finished_at not set")
	}
	want(t, "nothing to claim", e.call("POST", "/claim", "w1", nil, nil).status, 204)
}

func TestSecondDecisionIsRefused(t *testing.T) {
	e := newEnv(t)
	id := e.submitGated("twice")
	want(t, "first", e.call("POST", "/jobs/"+id+"/reject", "op1", nil, map[string]any{"reason": "no"}).status, 200)
	for _, call := range []struct {
		path string
		body any
	}{{"approve", nil}, {"reject", map[string]any{"reason": "again"}}} {
		r := e.call("POST", "/jobs/"+id+"/"+call.path, "op1", nil, call.body)
		want(t, call.path+" status", r.status, 409)
		want(t, call.path+" code", r.errCode(), "not_waiting_approval")
		d := r.json()["error"].(map[string]any)["details"].(map[string]any)
		want(t, call.path+" state", d["state"], "rejected")
		want(t, call.path+" decided_by", d["decided_by"], "op1")
	}

	// A job that never needed approval is simply not waiting; there is no decided_by.
	plain := e.mustSubmit("alice", "p", "plain")
	r := e.call("POST", "/jobs/"+plain+"/approve", "op1", nil, nil)
	want(t, "plain job", r.errCode(), "not_waiting_approval")
	d := r.json()["error"].(map[string]any)["details"].(map[string]any)
	want(t, "state", d["state"], "queued")
	if _, has := d["decided_by"]; has {
		t.Error("decided_by should be left out when nobody decided")
	}
}

func TestOperatorCallsNotFound(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		path string
		body any
	}{{"approve", nil}, {"reject", map[string]any{"reason": "x"}}, {"redrive", map[string]any{"max_attempts": 5}}} {
		for _, id := range []string{"00000000-0000-0000-0000-000000000000", "not-a-uuid"} {
			want(t, c.path+" "+id, e.call("POST", "/jobs/"+id+"/"+c.path, "op1", nil, c.body).status, 404)
		}
	}
}

// Several operators decide the same job at once: one wins, the rest get 409.
func TestConcurrentDecisions(t *testing.T) {
	e := newEnv(t)
	id := e.submitGated("race")
	var wg sync.WaitGroup
	codes := make([]int, 10)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			path, body := "approve", any(nil)
			if i%2 == 1 {
				path, body = "reject", map[string]any{"reason": "no"}
			}
			codes[i] = e.call("POST", "/jobs/"+id+"/"+path, "op1", nil, body).status
		}()
	}
	wg.Wait()
	ok, conflict := 0, 0
	for _, c := range codes {
		switch c {
		case 200:
			ok++
		case 409:
			conflict++
		}
	}
	if ok != 1 || conflict != 9 {
		t.Errorf("want one 200 and nine 409, got %v", codes)
	}
}

// ---------------------------------------------------------------------------
// Redrive
// ---------------------------------------------------------------------------

func TestRedrive(t *testing.T) {
	e := newEnv(t)
	id := e.makeDead("d1")

	r := e.call("POST", "/jobs/"+id+"/redrive", "op1", nil, map[string]any{"max_attempts": 3})
	want(t, "status", r.status, 200)
	j := r.json()
	want(t, "state", j["state"], "queued")
	want(t, "max_attempts", j["max_attempts"], float64(3))
	want(t, "attempt is not reset", j["attempt"], float64(1))
	want(t, "redriven_by", j["redriven_by"], "op1")
	want(t, "finished_at cleared", j["finished_at"], nil)
	if j["redriven_at"] == nil {
		t.Error("redriven_at not set")
	}
	want(t, "history kept", len(j["errors"].([]any)), 1)

	w := e.mustClaim("w1")
	want(t, "claimed again", w["id"], id)
	want(t, "attempt continues", w["attempt"], float64(2))

	// It is queued or running now, so a second redrive is refused.
	r = e.call("POST", "/jobs/"+id+"/redrive", "op1", nil, map[string]any{"max_attempts": 5})
	want(t, "not dead", r.status, 409)
	want(t, "code", r.errCode(), "not_dead")
}

func TestRedriveValidation(t *testing.T) {
	e := newEnv(t)
	id := e.makeDead("d2") // attempt 1

	cases := []struct {
		name   string
		body   any
		status int
		code   string
	}{
		{"equal to attempt", map[string]any{"max_attempts": 1}, 400, "max_attempts_too_low"},
		{"zero", map[string]any{"max_attempts": 0}, 400, "max_attempts_too_low"},
		{"101", map[string]any{"max_attempts": 101}, 400, "max_attempts_too_high"},
		{"huge", `{"max_attempts": 99999999999999999999}`, 400, "max_attempts_too_high"},
		{"missing", map[string]any{}, 400, "invalid_request"},
		{"not a number", map[string]any{"max_attempts": "5"}, 400, "invalid_request"},
		{"fraction", `{"max_attempts": 5.5}`, 400, "invalid_request"},
		{"no body", nil, 400, "invalid_request"},
	}
	for _, c := range cases {
		r := e.call("POST", "/jobs/"+id+"/redrive", "op1", nil, c.body)
		if r.status != c.status || r.errCode() != c.code {
			t.Errorf("%s: got %d %s, want %d %s", c.name, r.status, r.body, c.status, c.code)
		}
	}
	want(t, "still dead", e.job(id)["state"], "dead")
	r := e.call("POST", "/jobs/"+id+"/redrive", "op1", nil, map[string]any{"max_attempts": 1})
	want(t, "details.attempt", r.json()["error"].(map[string]any)["details"].(map[string]any)["attempt"], float64(1))

	// Only a dead job can be redriven.
	queued := e.mustSubmit("alice", "q", "plain")
	r = e.call("POST", "/jobs/"+queued+"/redrive", "op1", nil, map[string]any{"max_attempts": 5})
	want(t, "queued job", r.errCode(), "not_dead")
	want(t, "details.state", r.json()["error"].(map[string]any)["details"].(map[string]any)["state"], "queued")
}

// ---------------------------------------------------------------------------
// List
// ---------------------------------------------------------------------------

func (e *env) list(who, query string) resp {
	e.t.Helper()
	return e.call("GET", "/jobs"+query, who, nil, nil)
}

func ids(t *testing.T, r resp) []string {
	t.Helper()
	if r.status != 200 {
		t.Fatalf("list: %d %s", r.status, r.body)
	}
	var out []string
	for _, j := range r.json()["jobs"].([]any) {
		out = append(out, j.(map[string]any)["id"].(string))
	}
	return out
}

func TestListVisibilityAndFilters(t *testing.T) {
	e := newEnv(t)
	dead := e.makeDead("l4") // first, because a claim takes the oldest due job
	a1 := e.mustSubmit("alice", "l1", "plain")
	a2 := e.mustSubmit("alice", "l2", "two_tries")
	b1 := e.mustSubmit("bob", "l3", "plain")

	wantIDs(t, "client sees own, newest first", ids(t, e.list("alice", "")), []string{a2, a1, dead})
	wantIDs(t, "other client", ids(t, e.list("bob", "")), []string{b1})
	wantIDs(t, "client_id is ignored for a client", ids(t, e.list("bob", "?client_id=alice")), []string{b1})
	wantIDs(t, "operator sees all", ids(t, e.list("op1", "")), []string{b1, a2, a1, dead})
	wantIDs(t, "operator narrows by client", ids(t, e.list("op1", "?client_id=bob")), []string{b1})
	wantIDs(t, "state", ids(t, e.list("op1", "?state=dead")), []string{dead})
	wantIDs(t, "several states", ids(t, e.list("op1", "?state=dead,queued")), []string{b1, a2, a1, dead})
	wantIDs(t, "type", ids(t, e.list("op1", "?type=two_tries")), []string{a2})

	// A summary has no payload, and carries the newest error.
	j := e.list("op1", "?state=dead").json()["jobs"].([]any)[0].(map[string]any)
	if _, has := j["payload"]; has {
		t.Error("a summary should not carry the payload")
	}
	le := j["last_error"].(map[string]any)
	want(t, "last_error kind", le["kind"], "retryable")
	want(t, "last_error message", le["message"], "boom")
	want(t, "no error is null", e.list("op1", "?type=two_tries").json()["jobs"].([]any)[0].(map[string]any)["last_error"], nil)

	empty := e.list("op1", "?state=succeeded")
	if !strings.Contains(string(empty.body), `"jobs":[]`) {
		t.Errorf("an empty list should be [] not null: %s", empty.body)
	}
}

func TestListEscalated(t *testing.T) {
	e := newEnv(t)
	late := e.submitGated("e1")
	e.submitGated("e2") // waiting, not overdue
	e.makeOverdue(late)
	wantIDs(t, "only the escalated one", ids(t, e.list("op1", "?escalated=true")), []string{late})
	want(t, "escalated=false is no filter", len(ids(t, e.list("op1", "?escalated=false"))), 2)
	want(t, "not with state", e.list("op1", "?escalated=true&state=dead").status, 400)
}

func TestListPaging(t *testing.T) {
	e := newEnv(t)
	var made []string
	for i := 0; i < 7; i++ {
		made = append(made, e.mustSubmit("alice", fmt.Sprintf("p%d", i), "plain"))
	}
	var got []string
	query := "?limit=3"
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("paging does not end")
		}
		r := e.list("alice", query)
		got = append(got, ids(t, r)...)
		next := r.json()["next_cursor"]
		if next == nil {
			break
		}
		query = "?limit=3&cursor=" + next.(string)
		if pages == 0 {
			// A job submitted between pages must not shift the next page.
			e.mustSubmit("alice", "late", "plain")
		}
	}
	var newestFirst []string
	for i := len(made) - 1; i >= 0; i-- {
		newestFirst = append(newestFirst, made[i])
	}
	wantIDs(t, "all seven, once each, newest first", got, newestFirst)

	// A page that is exactly full has no next page.
	r := e.list("bob", "?limit=1")
	want(t, "bob has none", r.json()["next_cursor"], nil)
	r = e.list("alice", "?limit=200")
	want(t, "one big page ends", r.json()["next_cursor"], nil)
}

func TestListBadParameters(t *testing.T) {
	e := newEnv(t)
	for _, q := range []string{
		"?state=nonsense", "?state=queued,nope", "?limit=0", "?limit=201", "?limit=x",
		"?cursor=garbage", "?cursor=e30", "?escalated=maybe",
		"?cursor=eyJjIjoiMjAyNi0xMC0wOVQyMDoxNTozMFoiLCJpIjoibm90LWEtdXVpZCJ9", // valid JSON, bad id
	} {
		r := e.list("op1", q)
		if r.status != 400 || r.errCode() != "invalid_request" {
			t.Errorf("%s: got %d %s", q, r.status, r.body)
		}
	}
}
