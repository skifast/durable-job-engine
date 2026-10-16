// Command worker claims jobs from the engine and runs them.
//
// Settings come from environment variables:
//
//	WORKER_KEY   this worker's API key, a key with the worker role (required).
//	             The key's name is the worker's name in lease_owner.
//	ENGINE_URL   the engine's address (default http://localhost:8080)
//
// This build registers the demo handlers in examples/handlers. A real deployment
// builds its own worker command with its own handlers, using internal/worker.
//
// SIGTERM or Ctrl-C starts a shutdown: no new jobs, and a running handler gets 30 s to
// finish. If a handler ignores its cancellation the worker exits with status 2, and
// something must restart it (ADR 014).
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/skifast/durable-job-engine/examples/handlers"
	"github.com/skifast/durable-job-engine/internal/worker"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	key := os.Getenv("WORKER_KEY")
	if key == "" {
		log.Error("WORKER_KEY must be set")
		os.Exit(1)
	}
	url := os.Getenv("ENGINE_URL")
	if url == "" {
		url = "http://localhost:8080"
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	w := &worker.Worker{
		Client:   &worker.Client{BaseURL: url, Key: key},
		Handlers: handlers.All(),
		Log:      log,
	}
	log.Info("worker started", "engine", url)
	err := w.Run(ctx)
	switch {
	case err == nil:
		log.Info("worker stopped")
	case errors.Is(err, worker.ErrHandlerStuck):
		log.Error("worker stopped", "error", err)
		os.Exit(2) // distinct from a configuration error, so a supervisor's logs show why
	default:
		log.Error("worker stopped", "error", err)
		os.Exit(1)
	}
}
