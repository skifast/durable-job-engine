package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/skifast/durable-job-engine/internal/api"
	"github.com/skifast/durable-job-engine/internal/config"
	"github.com/skifast/durable-job-engine/internal/store"
	"github.com/skifast/durable-job-engine/internal/worker"
	"github.com/skifast/durable-job-engine/testutil"
)

// A real engine API on a real database, with a hook to make requests fail on the way in.
type env struct {
	t    *testing.T
	pool *pgxpool.Pool
	st   *store.Store
	srv  *httptest.Server
	// intercept, if set, may answer a request itself. It returns true when it did.
	intercept atomic.Pointer[func(w http.ResponseWriter, r *http.Request, next http.Handler) bool]
	n         int
}

func newEnv(t *testing.T) *env {
	pool := testutil.NewDB(t)
	for name, role := range map[string]string{"alice": "client", "w1": "worker"} {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO api_keys (name, role, key_hash) VALUES ($1, $2, $3)`, name, role, store.HashKey("token-"+name)); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := config.Parse([]byte(`{"job_types": {
		"ok":    {},
		"short": {"handler_time_limit": "300ms"}
	}}`))
	if err != nil {
		t.Fatal(err)
	}
	st := store.New(pool)
	a := &api.API{Store: st, Config: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	e := &env{t: t, pool: pool, st: st}
	inner := a.Handler()
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f := e.intercept.Load(); f != nil && (*f)(w, r, inner) {
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(e.srv.Close)
	t.Cleanup(func() { testutil.CheckInvariants(t, pool) })
	return e
}

func (e *env) setIntercept(f func(w http.ResponseWriter, r *http.Request, next http.Handler) bool) {
	e.intercept.Store(&f)
}

func (e *env) submit(typ string, payload any) string {
	e.t.Helper()
	e.n++
	raw, _ := json.Marshal(payload)
	key := fmt.Sprintf("k%d", e.n)
	j, _, err := e.st.Submit(context.Background(), store.SubmitParams{
		ClientID: "alice", IdempotencyKey: key, Type: typ, Payload: raw,
		PayloadHash: store.HashKey(key), MaxAttempts: 8,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return j.ID
}

func (e *env) job(id string) *store.Job {
	e.t.Helper()
	j, err := e.st.GetJob(context.Background(), id, "")
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

// newWorker returns a worker with timings short enough for tests.
func (e *env) newWorker(h map[string]worker.Handler) *worker.Worker {
	return &worker.Worker{
		Client:            &worker.Client{BaseURL: e.srv.URL, Key: "token-w1"},
		Handlers:          h,
		Log:               slog.New(slog.NewTextHandler(io.Discard, nil)),
		PollInterval:      5 * time.Millisecond,
		HeartbeatInterval: 40 * time.Millisecond,
		LeaseLength:       400 * time.Millisecond,
		DrainWindow:       300 * time.Millisecond,
		StuckGrace:        200 * time.Millisecond,
		ReportRetryFor:    3 * time.Second,
		ErrorPause:        10 * time.Millisecond,
	}
}

// start runs the worker. The returned function asks it to shut down and returns what Run returned.
func (e *env) start(w *worker.Worker) (stop func() error) {
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- w.Run(ctx) }()
	var once sync.Once
	var result error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case result = <-errc:
			case <-time.After(5 * time.Second):
				e.t.Error("the worker did not stop")
			}
		})
		return result
	}
	e.t.Cleanup(func() { stop() })
	return stop
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func (e *env) waitState(id, state string) *store.Job {
	e.t.Helper()
	eventually(e.t, "job "+state, func() bool { return e.job(id).State == state })
	return e.job(id)
}

// waitErrors waits until the job has an entry in its error history. A retried job is
// queued again, so waiting for the state "queued" would return before the worker acted.
func (e *env) waitErrors(id string) *store.Job {
	e.t.Helper()
	eventually(e.t, "an error entry", func() bool { return len(e.job(id).Errors) > 0 })
	return e.job(id)
}

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %v, want %v", what, got, want)
	}
}

// blockUntilCancelled is a handler that does what a handler should: it stops when told to.
// It reports that it started, and why it was cancelled.
func blockUntilCancelled(started chan<- struct{}, cause chan<- error) worker.Handler {
	return func(ctx context.Context, job worker.Job) (any, error) {
		started <- struct{}{}
		<-ctx.Done()
		cause <- context.Cause(ctx)
		return nil, ctx.Err()
	}
}

// ---------------------------------------------------------------------------
// Outcomes
// ---------------------------------------------------------------------------

func TestSuccessWithResult(t *testing.T) {
	e := newEnv(t)
	id := e.submit("ok", map[string]any{"n": 7})
	got := make(chan worker.Job, 1)
	e.start(e.newWorker(map[string]worker.Handler{"ok": func(ctx context.Context, j worker.Job) (any, error) {
		got <- j
		return map[string]any{"sent": true}, nil
	}}))
	j := e.waitState(id, "succeeded")
	eq(t, "result", string(j.Result), `{"sent": true}`)
	job := <-got
	eq(t, "job id", job.ID, id)
	eq(t, "job type", job.Type, "ok")
	eq(t, "attempt", job.Attempt, 1)
	eq(t, "payload", string(job.Payload), `{"n":7}`)
}

func TestSuccessWithNoResult(t *testing.T) {
	e := newEnv(t)
	id := e.submit("ok", nil)
	e.start(e.newWorker(map[string]worker.Handler{"ok": func(context.Context, worker.Job) (any, error) { return nil, nil }}))
	eq(t, "result", string(e.waitState(id, "succeeded").Result), `{}`)
}

// The handler picks its failure from the payload. One worker runs all four jobs, so this
// also shows a panic does not kill the worker.
func TestFailureKinds(t *testing.T) {
	e := newEnv(t)
	ids := map[string]string{}
	for _, mode := range []string{"retryable", "permanent", "plain", "panic", "wrapped"} {
		ids[mode] = e.submit("ok", map[string]string{"mode": mode})
	}
	e.start(e.newWorker(map[string]worker.Handler{"ok": func(ctx context.Context, j worker.Job) (any, error) {
		var p struct{ Mode string }
		json.Unmarshal(j.Payload, &p)
		switch p.Mode {
		case "retryable":
			return nil, worker.Retryable(errors.New("downstream timeout"))
		case "permanent":
			return nil, worker.Permanent(errors.New("bad invoice id"))
		case "plain":
			return nil, errors.New("something odd")
		case "wrapped": // classification survives fmt.Errorf("%w")
			return nil, fmt.Errorf("while sending: %w", worker.Permanent(errors.New("404 from mailer")))
		}
		panic("boom")
	}}))

	check := func(mode, state, kind, msgPart string) {
		t.Helper()
		j := e.waitErrors(ids[mode])
		eq(t, mode+" state", j.State, state)
		if len(j.Errors) != 1 {
			t.Fatalf("%s: %d error entries", mode, len(j.Errors))
		}
		eq(t, mode+" kind", j.Errors[0].Kind, kind)
		if !strings.Contains(j.Errors[0].Message, msgPart) {
			t.Errorf("%s: message %q lacks %q", mode, j.Errors[0].Message, msgPart)
		}
	}
	check("retryable", "queued", "retryable", "downstream timeout")
	check("permanent", "dead", "permanent", "bad invoice id")
	check("plain", "queued", "unclassified", "*errors.errorString: something odd") // type and message
	check("panic", "queued", "unclassified", "panic: boom")
	check("wrapped", "dead", "permanent", "while sending")
}

func TestNoHandlerIsRetryable(t *testing.T) {
	e := newEnv(t)
	id := e.submit("ok", nil)
	e.start(e.newWorker(map[string]worker.Handler{}))
	j := e.waitErrors(id)
	eq(t, "kind", j.Errors[0].Kind, "retryable")
	eq(t, "message", j.Errors[0].Message, "no handler for type ok")
}

func TestResultTooLargeBecomesPermanent(t *testing.T) {
	e := newEnv(t)
	id := e.submit("ok", nil)
	e.start(e.newWorker(map[string]worker.Handler{"ok": func(context.Context, worker.Job) (any, error) {
		return strings.Repeat("x", 70*1024), nil
	}}))
	j := e.waitState(id, "dead")
	eq(t, "kind", j.Errors[0].Kind, "permanent")
	if !strings.Contains(j.Errors[0].Message, "64 KB") {
		t.Errorf("message: %q", j.Errors[0].Message)
	}
}

func TestResultThatCannotBeEncodedIsPermanent(t *testing.T) {
	e := newEnv(t)
	id := e.submit("ok", nil)
	e.start(e.newWorker(map[string]worker.Handler{"ok": func(context.Context, worker.Job) (any, error) {
		return make(chan int), nil // json cannot encode a channel
	}}))
	j := e.waitState(id, "dead")
	eq(t, "kind", j.Errors[0].Kind, "permanent")
}

// ---------------------------------------------------------------------------
// Time limit, stuck handlers
// ---------------------------------------------------------------------------

func TestTimeLimitCancelsAndReportsRetryable(t *testing.T) {
	e := newEnv(t)
	id := e.submit("short", nil) // limit 300 ms, from the engine's config
	started, cause := make(chan struct{}, 1), make(chan error, 1)
	stop := e.start(e.newWorker(map[string]worker.Handler{"short": blockUntilCancelled(started, cause)}))
	<-started
	j := e.waitErrors(id)
	eq(t, "state", j.State, "queued")
	eq(t, "kind", j.Errors[0].Kind, "retryable")
	if !strings.Contains(j.Errors[0].Message, "time limit of 300ms") {
		t.Errorf("message: %q", j.Errors[0].Message)
	}
	if c := <-cause; !errors.Is(c, worker.ErrTimeLimit) {
		t.Errorf("cause: %v", c)
	}
	eq(t, "worker keeps running", stop(), error(nil))
}

func TestStuckHandlerMakesTheWorkerExit(t *testing.T) {
	e := newEnv(t)
	id := e.submit("short", nil)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	w := e.newWorker(map[string]worker.Handler{"short": func(context.Context, worker.Job) (any, error) {
		<-release // ignores ctx
		return nil, nil
	}})
	errc := make(chan error, 1)
	go func() { errc <- w.Run(context.Background()) }()
	select {
	case err := <-errc:
		if !errors.Is(err, worker.ErrHandlerStuck) {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the worker did not exit")
	}
	// The timeout was reported before the exit.
	j := e.waitErrors(id)
	eq(t, "state", j.State, "queued")
	eq(t, "kind", j.Errors[0].Kind, "retryable")
}

// ---------------------------------------------------------------------------
// Lease
// ---------------------------------------------------------------------------

func TestLostLeaseCancelsHandlerWithoutReporting(t *testing.T) {
	e := newEnv(t)
	id := e.submit("ok", nil)
	started, cause := make(chan struct{}, 1), make(chan error, 1)
	e.start(e.newWorker(map[string]worker.Handler{"ok": blockUntilCancelled(started, cause)}))
	<-started

	// The reaper takes the job back while the handler runs.
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if n, err := e.st.ReapExpiredLeases(context.Background(), 100); err != nil || n != 1 {
		t.Fatalf("reaped %d, err %v", n, err)
	}

	select {
	case c := <-cause:
		if !errors.Is(c, worker.ErrLeaseGone) {
			t.Errorf("cause: %v", c)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the handler was not cancelled")
	}
	time.Sleep(100 * time.Millisecond) // a wrong report would arrive about now
	j := e.job(id)
	eq(t, "state", j.State, "queued")
	eq(t, "only the reaper's entry", len(j.Errors), 1)
	eq(t, "kind", j.Errors[0].Kind, "lease_expired")
}

func TestSilentHeartbeatsCancelHandlerAfterALeaseLength(t *testing.T) {
	e := newEnv(t)
	id := e.submit("ok", nil)
	started, cause := make(chan struct{}, 1), make(chan error, 1)
	e.start(e.newWorker(map[string]worker.Handler{"ok": blockUntilCancelled(started, cause)}))
	<-started
	cutAt := time.Now()
	e.setIntercept(func(w http.ResponseWriter, r *http.Request, _ http.Handler) bool {
		if strings.HasSuffix(r.URL.Path, "/heartbeat") {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return true
		}
		return false
	})
	select {
	case c := <-cause:
		if !errors.Is(c, worker.ErrLeaseGone) {
			t.Errorf("cause: %v", c)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the handler was not cancelled")
	}
	if d := time.Since(cutAt); d > 1500*time.Millisecond {
		t.Errorf("took %v; the lease length is 400ms", d)
	}
	time.Sleep(100 * time.Millisecond)
	j := e.job(id)
	eq(t, "no report was sent", j.State, "running") // the reaper would take it back later
	eq(t, "no error entries", len(j.Errors), 0)
}

// ---------------------------------------------------------------------------
// Shutdown
// ---------------------------------------------------------------------------

func TestShutdownLetsARunningHandlerFinish(t *testing.T) {
	e := newEnv(t)
	id := e.submit("ok", nil)
	started := make(chan struct{}, 1)
	stop := e.start(e.newWorker(map[string]worker.Handler{"ok": func(context.Context, worker.Job) (any, error) {
		started <- struct{}{}
		time.Sleep(150 * time.Millisecond) // less than the 300 ms drain window
		return "done", nil
	}}))
	<-started
	eq(t, "Run returns nil", stop(), error(nil))
	eq(t, "state", e.job(id).State, "succeeded")
}

func TestShutdownCancelsAfterTheDrainWindow(t *testing.T) {
	e := newEnv(t)
	id := e.submit("ok", nil)
	started, cause := make(chan struct{}, 1), make(chan error, 1)
	stop := e.start(e.newWorker(map[string]worker.Handler{"ok": blockUntilCancelled(started, cause)}))
	<-started
	eq(t, "Run returns nil", stop(), error(nil))
	if c := <-cause; !errors.Is(c, worker.ErrShutdown) {
		t.Errorf("cause: %v", c)
	}
	j := e.waitErrors(id)
	eq(t, "state", j.State, "queued")
	eq(t, "kind", j.Errors[0].Kind, "retryable")
	eq(t, "message", j.Errors[0].Message, "worker shutting down")
}

func TestShutdownWhileIdleReturnsAtOnce(t *testing.T) {
	e := newEnv(t)
	stop := e.start(e.newWorker(map[string]worker.Handler{}))
	time.Sleep(30 * time.Millisecond)
	start := time.Now()
	eq(t, "Run returns nil", stop(), error(nil))
	if d := time.Since(start); d > time.Second {
		t.Errorf("took %v", d)
	}
}

// ---------------------------------------------------------------------------
// Network trouble
// ---------------------------------------------------------------------------

// The first report reaches the engine and is applied, but the answer is lost. The worker
// sends it again; the engine answers "replayed" and nothing is recorded twice.
func TestReportRetriedAfterALostAnswer(t *testing.T) {
	e := newEnv(t)
	id := e.submit("ok", nil)
	var lost atomic.Bool
	var reports atomic.Int32
	e.setIntercept(func(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
		if strings.HasSuffix(r.URL.Path, "/report") {
			reports.Add(1)
		}
		if strings.HasSuffix(r.URL.Path, "/report") && lost.CompareAndSwap(false, true) {
			next.ServeHTTP(httptest.NewRecorder(), r) // applied, answer thrown away
			http.Error(w, "gateway", http.StatusBadGateway)
			return true
		}
		return false
	})
	e.start(e.newWorker(map[string]worker.Handler{"ok": func(context.Context, worker.Job) (any, error) {
		return nil, worker.Retryable(errors.New("try later"))
	}}))
	eventually(t, "the worker to send the report again", func() bool { return reports.Load() >= 2 })
	time.Sleep(100 * time.Millisecond)
	j := e.job(id)
	eq(t, "state", j.State, "queued")
	eq(t, "one error entry", len(j.Errors), 1)
}

func TestReportRetriedWhileEngineIsDown(t *testing.T) {
	e := newEnv(t)
	id := e.submit("ok", nil)
	var fails atomic.Int32
	e.setIntercept(func(w http.ResponseWriter, r *http.Request, _ http.Handler) bool {
		if strings.HasSuffix(r.URL.Path, "/report") && fails.Add(1) <= 3 {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return true
		}
		return false
	})
	e.start(e.newWorker(map[string]worker.Handler{"ok": func(context.Context, worker.Job) (any, error) { return 1, nil }}))
	e.waitState(id, "succeeded")
	if fails.Load() < 4 {
		t.Errorf("only %d report requests; the worker should have retried", fails.Load())
	}
}

func TestClaimFailuresAreRetried(t *testing.T) {
	e := newEnv(t)
	id := e.submit("ok", nil)
	e.setIntercept(func(w http.ResponseWriter, r *http.Request, _ http.Handler) bool {
		if r.URL.Path == "/claim" {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return true
		}
		return false
	})
	e.start(e.newWorker(map[string]worker.Handler{"ok": func(context.Context, worker.Job) (any, error) { return 1, nil }}))
	time.Sleep(100 * time.Millisecond)
	eq(t, "nothing claimed while the engine is down", e.job(id).State, "queued")
	e.setIntercept(func(http.ResponseWriter, *http.Request, http.Handler) bool { return false })
	e.waitState(id, "succeeded")
}

func TestRejectedKeyStopsTheWorker(t *testing.T) {
	e := newEnv(t)
	w := e.newWorker(map[string]worker.Handler{})
	w.Client.Key = "not-a-key"
	errc := make(chan error, 1)
	go func() { errc <- w.Run(context.Background()) }()
	select {
	case err := <-errc:
		if !errors.Is(err, worker.ErrRejectedKey) {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the worker kept polling with a rejected key")
	}
}

// ---------------------------------------------------------------------------
// Classification helpers
// ---------------------------------------------------------------------------

func TestRetryableAndPermanentOfNilAreNil(t *testing.T) {
	if worker.Retryable(nil) != nil || worker.Permanent(nil) != nil {
		t.Error("wrapping nil must give nil, or a handler's success would become a failure")
	}
	inner := errors.New("x")
	if !errors.Is(worker.Retryable(inner), inner) {
		t.Error("the wrapped error should still be found with errors.Is")
	}
}
