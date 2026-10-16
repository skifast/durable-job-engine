package reaper_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/skifast/durable-job-engine/internal/reaper"
	"github.com/skifast/durable-job-engine/internal/store"
	"github.com/skifast/durable-job-engine/testutil"
)

type env struct {
	t    *testing.T
	pool *pgxpool.Pool
	st   *store.Store
	n    int
}

func newEnv(t *testing.T) *env {
	pool := testutil.NewDB(t)
	for name, role := range map[string]string{"alice": "client", "w1": "worker"} {
		_, err := pool.Exec(context.Background(),
			`INSERT INTO api_keys (name, role, key_hash) VALUES ($1, $2, $3)`, name, role, store.HashKey("token-"+name))
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { testutil.CheckInvariants(t, pool) })
	return &env{t: t, pool: pool, st: store.New(pool)}
}

func (e *env) submit(maxAttempts int, approvalWindow string) string {
	e.t.Helper()
	e.n++
	p := store.SubmitParams{
		ClientID: "alice", IdempotencyKey: string(rune('a' + e.n)), Type: "t",
		Payload: []byte(`{}`), PayloadHash: store.HashKey(string(rune('a' + e.n))), MaxAttempts: maxAttempts,
	}
	if approvalWindow != "" {
		p.ApprovalWindow = 1 // one nanosecond: the deadline is passed almost at once
	}
	j, _, err := e.st.Submit(context.Background(), p)
	if err != nil {
		e.t.Fatal(err)
	}
	return j.ID
}

// expire makes a running job's lease already over. A running -> running update is a
// legal heartbeat-shaped change, so no trigger trick is needed.
func (e *env) expire(id string) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) claim() *store.WorkItem {
	e.t.Helper()
	w, err := e.st.Claim(context.Background(), "w1")
	if err != nil || w == nil {
		e.t.Fatalf("claim: %v %v", w, err)
	}
	return w
}

func (e *env) job(id string) *store.Job {
	e.t.Helper()
	j, err := e.st.GetJob(context.Background(), id, "")
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

func (e *env) reap(limit int) int {
	e.t.Helper()
	n, err := e.st.ReapExpiredLeases(context.Background(), limit)
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestRequeuesExpiredLease(t *testing.T) { // T12
	e := newEnv(t)
	id := e.submit(3, "")
	e.claim()
	e.expire(id)
	if n := e.reap(100); n != 1 {
		t.Fatalf("reaped %d, want 1", n)
	}
	j := e.job(id)
	if j.State != "queued" || j.LeaseOwner != nil || j.LeaseExpiresAt != nil {
		t.Errorf("want queued with no lease, got %s owner=%v", j.State, j.LeaseOwner)
	}
	if !j.RunAt.Std().After(time.Now()) { // the first retry delay is about 2 s (ADR 004)
		t.Errorf("run_at %v should be in the future (backoff)", j.RunAt.Std())
	}
	if len(j.Errors) != 1 || j.Errors[0].Kind != "lease_expired" || j.Errors[0].Attempt != 1 {
		t.Errorf("error history: %+v", j.Errors)
	}
}

func TestDeadWhenAttemptsUsedUp(t *testing.T) { // T13
	e := newEnv(t)
	id := e.submit(1, "")
	e.claim()
	e.expire(id)
	e.reap(100)
	j := e.job(id)
	if j.State != "dead" || j.FinishedAt == nil {
		t.Errorf("want dead with finished_at, got %s", j.State)
	}
}

func TestLeaseStillValidIsLeftAlone(t *testing.T) {
	e := newEnv(t)
	id := e.submit(3, "")
	e.claim()
	if n := e.reap(100); n != 0 {
		t.Errorf("reaped %d jobs with a live lease", n)
	}
	if e.job(id).State != "running" {
		t.Error("job should still be running")
	}
}

// Running the reaper again, or two reapers at once, requeues a job once (spec section 4).
func TestReapTwiceRequeuesOnce(t *testing.T) {
	e := newEnv(t)
	id := e.submit(3, "")
	e.claim()
	e.expire(id)
	if e.reap(100) != 1 || e.reap(100) != 0 {
		t.Error("second pass should find nothing")
	}

	id2 := e.submit(3, "")
	e.claim()
	e.expire(id2)
	var wg sync.WaitGroup
	counts := make([]int, 8)
	for i := range counts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := e.st.ReapExpiredLeases(context.Background(), 100)
			if err != nil {
				t.Error(err)
			}
			counts[i] = n
		}()
	}
	wg.Wait()
	total := 0
	for _, c := range counts {
		total += c
	}
	if total != 1 {
		t.Errorf("8 concurrent reapers changed %d jobs, want 1", total)
	}
	if got := len(e.job(id2).Errors); got != 1 {
		t.Errorf("%d error entries, want 1", got)
	}
}

func TestBatchLimit(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 5; i++ {
		id := e.submit(3, "")
		e.claim()
		e.expire(id)
	}
	if n := e.reap(2); n != 2 {
		t.Errorf("first pass %d, want 2", n)
	}
	if n := e.reap(100); n != 3 {
		t.Errorf("second pass %d, want the other 3", n)
	}
}

// A job whose lease was renewed is not taken back. This does not hit the narrow window
// between the candidate list and the row lock; the re-check there is the same WHERE
// clause, so this covers its condition, not its timing.
func TestRenewedLeaseIsNotReaped(t *testing.T) {
	e := newEnv(t)
	id := e.submit(3, "")
	w := e.claim()
	e.expire(id)
	if _, err := e.st.Heartbeat(context.Background(), id, "w1", w.Attempt); err != nil {
		t.Fatal(err)
	}
	if n := e.reap(100); n != 0 {
		t.Errorf("reaped a job with a renewed lease")
	}
	if e.job(id).State != "running" {
		t.Error("job should still be running")
	}
}

func TestEscalatesOverdueApprovalOnce(t *testing.T) { // T5
	e := newEnv(t)
	id := e.submit(3, "overdue")
	other := e.submit(3, "")
	_ = other
	n, err := e.st.EscalateOverdueApprovals(context.Background(), 100)
	if err != nil || n != 1 {
		t.Fatalf("escalated %d, err %v", n, err)
	}
	j := e.job(id)
	if j.State != "waiting_approval" || j.EscalatedAt == nil {
		t.Errorf("want waiting_approval with escalated_at, got %s %v", j.State, j.EscalatedAt)
	}
	first := j.EscalatedAt.Std()
	if n, _ := e.st.EscalateOverdueApprovals(context.Background(), 100); n != 0 {
		t.Errorf("escalated a job twice")
	}
	if !e.job(id).EscalatedAt.Std().Equal(first) {
		t.Error("escalated_at changed")
	}
}

func TestFutureDeadlineNotEscalated(t *testing.T) {
	e := newEnv(t)
	e.n++
	_, _, err := e.st.Submit(context.Background(), store.SubmitParams{
		ClientID: "alice", IdempotencyKey: "k", Type: "t", Payload: []byte(`{}`), PayloadHash: store.HashKey("h"),
		MaxAttempts: 3, ApprovalWindow: 3600 * 1e9,
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := e.st.EscalateOverdueApprovals(context.Background(), 100); n != 0 {
		t.Errorf("escalated %d jobs before their deadline", n)
	}
}

func TestTickAndRunStopOnCancel(t *testing.T) {
	e := newEnv(t)
	id := e.submit(3, "")
	e.claim()
	e.expire(id)
	r := &reaper.Reaper{Store: e.st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	// Run ticks once at startup, so the job is taken back without waiting 5 s.
	for i := 0; i < 100 && e.job(id).State == "running"; i++ {
		sleep()
	}
	cancel()
	<-done
	if e.job(id).State != "queued" {
		t.Error("the startup tick did not requeue the job")
	}
}

func sleep() { time.Sleep(50 * time.Millisecond) }

// Spec section 6, "never happens": waiting_approval -> rejected by a timeout. Only an explicit
// reject call does that. However many times the reaper runs after the deadline, the job stays
// waiting, nobody is recorded as having decided, and escalated_at is set once and never moves.
func TestReaperNeverRejectsAnApproval(t *testing.T) {
	e := newEnv(t)
	id := e.submit(3, "overdue")
	r := &reaper.Reaper{Store: e.st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	var first time.Time
	for i := 0; i < 5; i++ {
		r.Tick(context.Background())
		j := e.job(id)
		if j.State != "waiting_approval" || j.DecidedBy != nil || j.DecidedAt != nil || j.DecisionReason != nil || j.FinishedAt != nil {
			t.Fatalf("pass %d: the reaper changed more than escalated_at: state %s, decided_by %v, finished_at %v", i+1, j.State, j.DecidedBy, j.FinishedAt)
		}
		if j.EscalatedAt == nil {
			t.Fatalf("pass %d: an overdue approval was not escalated", i+1)
		}
		if i == 0 {
			first = j.EscalatedAt.Std()
		} else if !j.EscalatedAt.Std().Equal(first) {
			t.Fatalf("pass %d: escalated_at moved from %v to %v", i+1, first, j.EscalatedAt.Std())
		}
	}
}

// Spec section 6, "never happens": escalating a job that is not waiting_approval. A job whose
// deadline has passed but which was approved, rejected, or has since been claimed is left alone,
// while a waiting job next to them is escalated (so the function does work).
func TestEscalationSkipsJobsThatAreNotWaiting(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	runningID := e.submit(3, "overdue")
	approvedID := e.submit(3, "overdue")
	rejectedID := e.submit(3, "overdue")
	waitingID := e.submit(3, "overdue")

	// Approve one and claim it at once (it is the only queued job), so it is running.
	if _, err := e.st.Approve(ctx, runningID, "op1", ""); err != nil {
		t.Fatal(err)
	}
	if w := e.claim(); w.ID != runningID {
		t.Fatalf("claimed %s, want %s", w.ID, runningID)
	}
	if _, err := e.st.Approve(ctx, approvedID, "op1", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.Reject(ctx, rejectedID, "op1", "no"); err != nil {
		t.Fatal(err)
	}

	n, err := e.st.EscalateOverdueApprovals(ctx, 100)
	if err != nil || n != 1 {
		t.Fatalf("escalated %d jobs, want only the one still waiting (err %v)", n, err)
	}
	for name, id := range map[string]string{"running": runningID, "approved (queued)": approvedID, "rejected": rejectedID} {
		if j := e.job(id); j.EscalatedAt != nil {
			t.Errorf("%s job was escalated", name)
		}
	}
	if e.job(waitingID).EscalatedAt == nil {
		t.Error("the waiting job was not escalated")
	}
	if e.job(runningID).State != "running" || e.job(approvedID).State != "queued" || e.job(rejectedID).State != "rejected" {
		t.Error("the escalation pass changed a state")
	}
}

// ---------------------------------------------------------------------------
// Several reapers at once (ADR 016)
// ---------------------------------------------------------------------------
//
// With several engines, every engine runs a reaper. Their timers are not in step, but nothing
// stops two from running at the same instant, so the safety of that instant is what is tested:
// every reaper is held at a barrier and released together, round after round.

// round releases n copies of fn at the same moment and returns what each returned.
func round(n int, fn func() (int, error)) ([]int, []error) {
	start := make(chan struct{})
	counts := make([]int, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			counts[i], errs[i] = fn()
		}()
	}
	close(start)
	wg.Wait()
	return counts, errs
}

func (e *env) manyRunningWithExpiredLeases(n int) {
	e.t.Helper()
	for i := 0; i < n; i++ {
		e.submit(3, "")
		e.claim()
	}
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE state = 'running'`); err != nil {
		e.t.Fatal(err)
	}
}

// 1,000 expired leases, three reapers released together each round. Every job must be requeued
// exactly once: the counts add up to 1,000, each job has exactly one lease_expired entry, and
// no round returns an error (a double requeue would break a constraint and surface here).
func TestThreeReapersFiringTogetherRequeueEachJobOnce(t *testing.T) {
	const jobs, reapers = 1000, 3
	e := newEnv(t)
	ctx := context.Background()
	e.manyRunningWithExpiredLeases(jobs)

	total, rounds := 0, 0
	t0 := time.Now()
	for ; rounds < 100; rounds++ {
		counts, errs := round(reapers, func() (int, error) { return e.st.ReapExpiredLeases(ctx, 100) })
		for _, err := range errs {
			if err != nil {
				t.Fatalf("round %d: a reaper failed: %v", rounds+1, err)
			}
		}
		this := 0
		for _, c := range counts {
			this += c
		}
		total += this
		if this == 0 {
			break
		}
	}
	elapsed := time.Since(t0)

	if total != jobs {
		t.Errorf("the reapers reported %d requeues in all, want %d", total, jobs)
	}
	var entries, distinct, queued, notOnce int
	e.pool.QueryRow(ctx, `SELECT count(*), count(DISTINCT job_id) FROM job_errors WHERE kind = 'lease_expired'`).Scan(&entries, &distinct)
	e.pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE state = 'queued'`).Scan(&queued)
	e.pool.QueryRow(ctx, `SELECT count(*) FROM (SELECT job_id FROM job_errors GROUP BY job_id HAVING count(*) <> 1) x`).Scan(&notOnce)
	if entries != jobs || distinct != jobs || notOnce != 0 {
		t.Errorf("lease_expired entries %d for %d distinct jobs, %d jobs without exactly one entry; want %d, %d, 0", entries, distinct, notOnce, jobs, jobs)
	}
	if queued != jobs {
		t.Errorf("%d jobs queued, want %d", queued, jobs)
	}
	t.Logf("%d reapers released together recovered %d expired leases in %d rounds (%v)", reapers, jobs, rounds, elapsed.Round(time.Millisecond))
}

// The same for the escalation: three reapers escalating 500 overdue approvals at the same
// moment. Each is escalated once, and escalated_at never moves afterwards (the database refuses a
// second escalation, so a double would show up as an error).
func TestThreeReapersEscalatingTogetherEscalateEachJobOnce(t *testing.T) {
	const jobs, reapers = 500, 3
	e := newEnv(t)
	ctx := context.Background()
	for i := 0; i < jobs; i++ {
		e.submit(3, "overdue")
	}

	total := 0
	for r := 0; r < 100; r++ {
		counts, errs := round(reapers, func() (int, error) { return e.st.EscalateOverdueApprovals(ctx, 100) })
		for _, err := range errs {
			if err != nil {
				t.Fatalf("round %d: a reaper failed: %v", r+1, err)
			}
		}
		this := 0
		for _, c := range counts {
			this += c
		}
		total += this
		if this == 0 {
			break
		}
	}
	if total != jobs {
		t.Errorf("the reapers reported %d escalations in all, want %d", total, jobs)
	}

	snapshot := func() map[string]time.Time {
		rows, err := e.pool.Query(ctx, `SELECT id::text, escalated_at FROM jobs WHERE state = 'waiting_approval' AND escalated_at IS NOT NULL`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		m := map[string]time.Time{}
		for rows.Next() {
			var id string
			var at time.Time
			rows.Scan(&id, &at)
			m[id] = at
		}
		return m
	}
	before := snapshot()
	if len(before) != jobs {
		t.Fatalf("%d jobs escalated and still waiting, want %d", len(before), jobs)
	}
	// Another full round changes nothing.
	counts, errs := round(reapers, func() (int, error) { return e.st.EscalateOverdueApprovals(ctx, 100) })
	for i := range counts {
		if counts[i] != 0 || errs[i] != nil {
			t.Errorf("a later pass escalated %d (error %v)", counts[i], errs[i])
		}
	}
	for id, at := range snapshot() {
		if !before[id].Equal(at) {
			t.Errorf("job %s: escalated_at moved from %v to %v", id, before[id], at)
		}
	}
}
