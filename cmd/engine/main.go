// Command engine runs the API server.
//
// Settings come from environment variables:
//
//	DATABASE_URL     Postgres connection string (required)
//	JOB_TYPES_FILE   path to the job-type JSON file (required)
//	LISTEN_ADDR      address to listen on (default :8080)
//
// The engine also runs the reaper (every 5 s, up to 100 jobs of each kind per pass).
//
// The engine speaks plain HTTP. Run it behind a TLS proxy, or API keys cross the
// network in the clear (ADR 013).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/skifast/durable-job-engine/internal/api"
	"github.com/skifast/durable-job-engine/internal/config"
	"github.com/skifast/durable-job-engine/internal/reaper"
	"github.com/skifast/durable-job-engine/internal/store"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("engine stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	dbURL := os.Getenv("DATABASE_URL")
	cfgPath := os.Getenv("JOB_TYPES_FILE")
	if dbURL == "" || cfgPath == "" {
		return errors.New("DATABASE_URL and JOB_TYPES_FILE must be set")
	}
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	st := store.New(pool)

	// The reaper runs in this process. It stops when ctx is cancelled, and we wait
	// for it below so a pass is not cut off halfway through a job.
	reaperDone := make(chan struct{})
	go func() {
		defer close(reaperDone)
		(&reaper.Reaper{Store: st, Log: log}).Run(ctx)
	}()

	a := &api.API{Store: st, Config: cfg, Log: log}
	srv := &http.Server{
		Addr:              addr,
		Handler:           a.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("engine listening", "addr", addr, "job_types", len(cfg.JobTypes))

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := srv.Shutdown(shutdownCtx) // lets requests in progress finish
		<-reaperDone
		return err
	}
}
