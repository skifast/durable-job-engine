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
