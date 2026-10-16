package api_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/skifast/durable-job-engine/internal/api"
	"github.com/skifast/durable-job-engine/internal/config"
	"github.com/skifast/durable-job-engine/internal/store"
)

// What /healthz notices matters once a load balancer decides from it where to send traffic
// (ADR 016). An engine that answers "ok" while it cannot serve would keep receiving requests.
// The first version ran SELECT 1, which does not touch any table; the lock test below is the
// case it missed (healthz answered 200 in 21 ms while a query on jobs hung).

func healthStatus(t *testing.T, url string) (int, time.Duration) {
	t.Helper()
	t0 := time.Now()
	res, err := http.Get(url + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode, time.Since(t0)
}

// A lock on the jobs table (a migration, for example) makes every real request wait. The engine
// must say so, and say "ok" again as soon as the lock is gone.
func TestHealthzNoticesALockedJobsTable(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	want(t, "before", func() int { s, _ := healthStatus(t, e.srv.URL); return s }(), 200)

	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE jobs IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	status, took := healthStatus(t, e.srv.URL)
	tx.Rollback(ctx)
	want(t, "while the table is locked", status, 503)
	if took > 3*time.Second {
		t.Errorf("took %v to say so; the check allows 2 s", took)
	}
	want(t, "after the lock is released", func() int { s, _ := healthStatus(t, e.srv.URL); return s }(), 200)
}

// Connections killed by the server (a Postgres restart, a failover, an idle-connection reaper)
// must not leave the engine reporting failure: the pool replaces them.
func TestHealthzHealsAfterItsConnectionsAreKilled(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid()`); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	var last int
	for i := 0; i < 5; i++ { // a first answer of 503 is acceptable; staying 503 is not
		if last, _ = healthStatus(t, e.srv.URL); last == 200 {
			return
		}
	}
	t.Errorf("still %d after five tries", last)
}

// An engine whose connections are all in use cannot take a new request either. The check gives up
// after 2 s and says so, instead of hanging the load balancer's probe.
func TestHealthzWhenEveryConnectionIsBusy(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cfg := e.pool.Config()
	cfg.MaxConns = 1
	small, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer small.Close()
	a := &api.API{Store: store.New(small), Config: &config.Config{}, Log: slog.New(slog.NewTextHandler(testLog{t}, nil))}
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	want(t, "idle", func() int { s, _ := healthStatus(t, srv.URL); return s }(), 200)

	busy, err := small.Acquire(ctx) // the pool's only connection
	if err != nil {
		t.Fatal(err)
	}
	status, took := healthStatus(t, srv.URL)
	busy.Release()
	want(t, "every connection busy", status, 503)
	if took < time.Second || took > 3*time.Second {
		t.Errorf("answered after %v, expected the 2 s limit", took)
	}
	want(t, "after the connection is free", func() int { s, _ := healthStatus(t, srv.URL); return s }(), 200)
}
