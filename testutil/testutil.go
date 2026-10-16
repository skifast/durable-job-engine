// Package testutil gives tests a real Postgres.
//
// Tests run against a real database, not a mock, because the database enforces
// the rules (ADR 011): the CHECK constraints and triggers in db/schema.sql must
// be in force, or a test could pass while the real thing fails.
//
// Set TEST_DATABASE_URL to a connection string for a Postgres server where the
// user may create databases, for example:
//
//	docker run -d --name jobpg -e POSTGRES_PASSWORD=pw -p 5432:5432 postgres:16
//	export TEST_DATABASE_URL='postgres://postgres:pw@localhost:5432/postgres'
//
// Each call to NewDB creates its own empty database, loads db/schema.sql and
// db/invariants.sql into it, and drops it when the test ends.
package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewDB returns a pool on a fresh database with the schema and invariant views loaded.
func NewDB(t testing.TB) *pgxpool.Pool {
	t.Helper()
	adminURL := os.Getenv("TEST_DATABASE_URL")
	if adminURL == "" {
		t.Fatal("TEST_DATABASE_URL is not set. These tests need a real Postgres 13 or newer; see the comment at the top of testutil/testutil.go.")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect to TEST_DATABASE_URL: %v", err)
	}
	suffix := make([]byte, 6)
	rand.Read(suffix)
	name := "jobengine_test_" + hex.EncodeToString(suffix)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name+" ENCODING 'UTF8' TEMPLATE template0 LC_COLLATE 'C' LC_CTYPE 'C'"); err != nil {
		admin.Close()
		t.Fatalf("create test database: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	cfg.MaxConns = 20
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		// WITH (FORCE) drops the database even if a connection is still open.
		admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		admin.Close()
	})

	root := repoRoot(t)
	for _, f := range []string{"db/schema.sql", "db/invariants.sql"} {
		sql, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		// With no arguments pgx uses the simple protocol, which accepts a whole file of statements.
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("load %s: %v", f, err)
		}
	}
	return pool
}

// CheckInvariants fails the test if any row appears in the invariant_violations view.
func CheckInvariants(t testing.TB, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := pool.Query(ctx, `SELECT invariant, job_id::text, detail FROM invariant_violations`)
	if err != nil {
		t.Fatalf("invariant check: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var inv, id, detail string
		rows.Scan(&inv, &id, &detail)
		t.Errorf("invariant violated: %s (job %s): %s", inv, id, detail)
	}
}

// repoRoot finds the folder that holds go.mod, starting from this file.
func repoRoot(t testing.TB) string {
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find go.mod above testutil")
		}
		dir = parent
	}
}
