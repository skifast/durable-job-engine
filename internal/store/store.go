// Package store is the only code that talks to Postgres.
//
// The API handlers and the reaper call these functions and never write SQL
// themselves. That keeps one copy of each rule, for example the retry-or-dead
// decision that both /report and the reaper apply (spec section 6).
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/rand"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/skifast/durable-job-engine/internal/retry"
)

const (
	// DefaultLeaseLength is the lease a claim or a heartbeat grants (ADR 003).
	DefaultLeaseLength = 30 * time.Second
	// MaxMessageChars is the longest error message kept (spec section 5).
	MaxMessageChars = 1000
	// duplicateWait is how long a submit waits for another submit with the same
	// key that has not committed yet (ADR 005).
	duplicateWait = "2s"
)

// Store holds the connection pool and the settings the queries need.
type Store struct {
	Pool        *pgxpool.Pool
	LeaseLength time.Duration // tests shorten this

	rndMu sync.Mutex
	rnd   *rand.Rand // jitter source; a *rand.Rand is not safe for several goroutines, so it is guarded
}

// New returns a Store using pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{
		Pool:        pool,
		LeaseLength: DefaultLeaseLength,
		rnd:         rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// querier is what both a pool and a transaction can do, so one helper works with either.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// HashKey is the SHA-256 of an API key, in lowercase hex. Only this is stored (ADR 013).
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// Ping runs a trivial query, for /healthz.
func (s *Store) Ping(ctx context.Context) error {
	var one int
	return s.Pool.QueryRow(ctx, "SELECT 1").Scan(&one)
}

// LookupKey finds the caller for an API key. It returns false for an unknown or
// revoked key. It runs on every request and is not cached, so a revoke takes
// effect on the next request (ADR 013).
func (s *Store) LookupKey(ctx context.Context, key string) (Key, bool, error) {
	var k Key
	err := s.Pool.QueryRow(ctx,
		`SELECT name, role FROM api_keys WHERE key_hash = $1 AND revoked_at IS NULL`,
		HashKey(key)).Scan(&k.Name, &k.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return Key{}, false, nil
	}
	if err != nil {
		return Key{}, false, err
	}
	return k, true, nil
}

// ---------------------------------------------------------------------------
// Submit (T1, T2)
// ---------------------------------------------------------------------------

// SubmitParams is a validated submit. The API has already checked sizes, the
// job type and the key, and computed the hash.
type SubmitParams struct {
	ClientID       string
	IdempotencyKey string
	Type           string
	Payload        []byte // the payload as JSON text
	PayloadHash    string // SHA-256 of the canonical JSON of {type, payload}
	MaxAttempts    int
	// ApprovalWindow is zero for a job type that needs no approval. Otherwise
	// the job starts as waiting_approval with a deadline this far away.
	ApprovalWindow time.Duration
}

// Submit stores a job, or returns the earlier job for the same client and key.
// created is true for a new job (201) and false for a repeat (200).
// It returns ErrKeyReused if the key was used with a different request, and
// ErrInProgress if the first request with this key did not finish in time.
func (s *Store) Submit(ctx context.Context, p SubmitParams) (job *Job, created bool, err error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx) // does nothing after a commit

	// If another transaction is inserting the same key and has not committed,
	// the insert below waits for it. Wait at most 2 s (ADR 005). LOCAL means
	// the setting ends with this transaction.
	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '"+duplicateWait+"'"); err != nil {
		return nil, false, err
	}

	state := "queued"
	var approvalSecs *float64
	if p.ApprovalWindow > 0 {
		state = "waiting_approval"
		v := p.ApprovalWindow.Seconds()
		approvalSecs = &v
	}

	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO jobs (type, client_id, idempotency_key, payload_hash, payload, state, max_attempts, approval_deadline)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7,
		        CASE WHEN $8::float8 IS NULL THEN NULL ELSE now() + make_interval(secs => $8::float8) END)
		ON CONFLICT (client_id, idempotency_key) DO NOTHING
		RETURNING id`,
		p.Type, p.ClientID, p.IdempotencyKey, p.PayloadHash, string(p.Payload), state, p.MaxAttempts, approvalSecs,
	).Scan(&id)

	switch {
	case err == nil:
		created = true
	case errors.Is(err, pgx.ErrNoRows):
		// The key exists. It is a repeat if the request is the same.
		var existingHash string
		err = tx.QueryRow(ctx,
			`SELECT id, payload_hash FROM jobs WHERE client_id = $1 AND idempotency_key = $2`,
			p.ClientID, p.IdempotencyKey).Scan(&id, &existingHash)
		if err != nil {
			return nil, false, err
		}
		if existingHash != p.PayloadHash {
			return nil, false, ErrKeyReused
		}
	default:
		return nil, false, mapSubmitError(err)
	}

	job, err = loadJob(ctx, tx, id)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return job, created, nil
}

func mapSubmitError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "55P03": // lock_not_available: the 2 s lock_timeout ran out
			return ErrInProgress
		case "22P05", "22P02", "22021": // \u0000 in a string, lone surrogate, bad bytes
			return ErrBadContent
		}
	}
	return err
}

// ---------------------------------------------------------------------------
// Read
// ---------------------------------------------------------------------------

const jobColumns = `id, type, client_id, idempotency_key, state, attempt, max_attempts, run_at,
	lease_owner, lease_expires_at, approval_deadline, escalated_at,
	decided_by, decided_at, decision_reason, redriven_by, redriven_at,
	payload, result, created_at, updated_at, finished_at`

// loadJob reads one job with its error history.
func loadJob(ctx context.Context, q querier, id string) (*Job, error) {
	var j Job
	err := q.QueryRow(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = $1`, id).Scan(
		&j.ID, &j.Type, &j.ClientID, &j.IdempotencyKey, &j.State, &j.Attempt, &j.MaxAttempts, &j.RunAt,
		&j.LeaseOwner, &j.LeaseExpiresAt, &j.ApprovalDeadline, &j.EscalatedAt,
		&j.DecidedBy, &j.DecidedAt, &j.DecisionReason, &j.RedrivenBy, &j.RedrivenAt,
		&j.Payload, &j.Result, &j.CreatedAt, &j.UpdatedAt, &j.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	rows, err := q.Query(ctx,
		`SELECT attempt, kind, message, occurred_at FROM job_errors WHERE job_id = $1 ORDER BY attempt`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	j.Errors = []ErrorEntry{} // an empty list, not null
	for rows.Next() {
		var e ErrorEntry
		if err := rows.Scan(&e.Attempt, &e.Kind, &e.Message, &e.OccurredAt); err != nil {
			return nil, err
		}
		j.Errors = append(j.Errors, e)
	}
	return &j, rows.Err()
}

// GetJob returns one job. If clientID is not empty, the job must belong to that
// client; another client's job is reported as ErrNotFound, the same as a job
// that does not exist (ADR 013).
func (s *Store) GetJob(ctx context.Context, id, clientID string) (*Job, error) {
	j, err := loadJob(ctx, s.Pool, id)
	if err != nil {
		return nil, err
	}
	if clientID != "" && j.ClientID != clientID {
		return nil, ErrNotFound
	}
	return j, nil
}

// ---------------------------------------------------------------------------
// Claim (T6)
// ---------------------------------------------------------------------------

// Claim gives the calling worker the oldest due job, or returns nil if none is due.
//
// FOR UPDATE SKIP LOCKED makes two workers that claim at the same moment take
// different jobs instead of waiting for each other. The row lock lasts only until
// this statement commits, so the claim itself is written into the row: state,
// lease_owner, lease_expires_at and the new attempt number.
func (s *Store) Claim(ctx context.Context, worker string) (*WorkItem, error) {
	var w WorkItem
	err := s.Pool.QueryRow(ctx, `
		WITH next AS (
		    SELECT id FROM jobs
		    WHERE state = 'queued' AND run_at <= now()
		    ORDER BY run_at, id
		    FOR UPDATE SKIP LOCKED
		    LIMIT 1
		)
		UPDATE jobs j
		SET state = 'running',
		    attempt = j.attempt + 1,
		    lease_owner = $1,
		    lease_expires_at = now() + make_interval(secs => $2::float8)
		FROM next
		WHERE j.id = next.id
		RETURNING j.id, j.type, j.payload, j.attempt, j.lease_expires_at`,
		worker, s.LeaseLength.Seconds(),
	).Scan(&w.ID, &w.Type, &w.Payload, &w.Attempt, &w.LeaseExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// ---------------------------------------------------------------------------
// Heartbeat (T7)
// ---------------------------------------------------------------------------

// Heartbeat renews the lease. The UPDATE has the fence in its WHERE clause: if the
// job is not running, is held by another worker, or is at another attempt, it
// changes nothing and the caller gets a LeaseLostError. ErrNotFound means no such job.
func (s *Store) Heartbeat(ctx context.Context, id, worker string, attempt int) (Time, error) {
	var expires Time
	err := s.Pool.QueryRow(ctx, `
		UPDATE jobs
		SET lease_expires_at = now() + make_interval(secs => $4::float8)
		WHERE id = $1 AND state = 'running' AND lease_owner = $2 AND attempt = $3
		RETURNING lease_expires_at`,
		id, worker, attempt, s.LeaseLength.Seconds()).Scan(&expires)
	if err == nil {
		return expires, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Time{}, err
	}
	return Time{}, s.explainFenceMiss(ctx, id)
}

// explainFenceMiss turns "the fenced UPDATE touched no rows" into the right error.
func (s *Store) explainFenceMiss(ctx context.Context, id string) error {
	var state string
	err := s.Pool.QueryRow(ctx, `SELECT state FROM jobs WHERE id = $1`, id).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return &LeaseLostError{State: state}
}

// ---------------------------------------------------------------------------
// Report (T8 to T11)
// ---------------------------------------------------------------------------

// Outcomes a worker can report. The three failure outcomes are also the error kinds.
const (
	OutcomeSuccess      = "success"
	OutcomeRetryable    = "retryable"
	OutcomePermanent    = "permanent"
	OutcomeUnclassified = "unclassified"
)

// ReportInput is a validated report.
type ReportInput struct {
	JobID   string
	Worker  string // the calling key's name
	Attempt int    // the fencing token
	Outcome string
	Result  []byte // JSON text; with OutcomeSuccess only; nil is stored as {}
	Message string // with the failure outcomes
}

// ReportResult is the answer to a report (docs/api.md).
type ReportResult struct {
	State    string `json:"state"`
	Attempt  int    `json:"attempt"`
	RunAt    *Time  `json:"run_at"` // set only when the job was requeued
	Replayed bool   `json:"replayed"`
}

// Report applies a worker's outcome, in one transaction. The row is locked first
// (FOR UPDATE), so a second report for the same job waits and then sees the first one's result.
//
// A report is accepted when the job is running, held by this worker, at this attempt.
// It is accepted even if the lease time has passed but the reaper has not yet taken
// the job back: the fence is the attempt number, not the clock (ADR 009).
func (s *Store) Report(ctx context.Context, in ReportInput) (*ReportResult, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var state string
	var attempt, maxAttempts int
	var owner *string
	err = tx.QueryRow(ctx,
		`SELECT state, attempt, max_attempts, lease_owner FROM jobs WHERE id = $1 FOR UPDATE`, in.JobID,
	).Scan(&state, &attempt, &maxAttempts, &owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	holdsLease := state == "running" && owner != nil && *owner == in.Worker && attempt == in.Attempt
	if !holdsLease {
		// Maybe this exact report was already applied and the answer was lost.
		replay, err := s.isReplay(ctx, tx, in, state, attempt)
		if err != nil {
			return nil, err
		}
		if replay {
			res := &ReportResult{State: state, Attempt: attempt, Replayed: true}
			if state == "queued" {
				var runAt Time
				if err := tx.QueryRow(ctx, `SELECT run_at FROM jobs WHERE id = $1`, in.JobID).Scan(&runAt); err != nil {
					return nil, err
				}
				res.RunAt = &runAt
			}
			return res, nil
		}
		return nil, &LeaseLostError{State: state}
	}

	res := &ReportResult{Attempt: attempt}
	if in.Outcome == OutcomeSuccess {
		result := in.Result
		if result == nil {
			result = []byte("{}")
		}
		_, err = tx.Exec(ctx, `
			UPDATE jobs
			SET state = 'succeeded', result = $2::jsonb,
			    lease_owner = NULL, lease_expires_at = NULL, finished_at = now()
			WHERE id = $1`, in.JobID, string(result))
		if err != nil {
			return nil, mapSubmitError(err)
		}
		res.State = "succeeded"
	} else {
		state, runAt, err := s.endAttemptWithFailure(ctx, tx, in.JobID, attempt, maxAttempts, in.Outcome, in.Message)
		if err != nil {
			return nil, err
		}
		res.State, res.RunAt = state, runAt
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return res, nil
}

// isReplay reports whether in repeats an outcome that is already recorded for its attempt.
// For success: the job is succeeded at that attempt.
// For a failure: job_errors has an entry for that attempt with the same kind. A lease the
// reaper took back has kind lease_expired, so a late report after that is not a replay.
func (s *Store) isReplay(ctx context.Context, q querier, in ReportInput, state string, attempt int) (bool, error) {
	if in.Outcome == OutcomeSuccess {
		return state == "succeeded" && attempt == in.Attempt, nil
	}
	var kind string
	err := q.QueryRow(ctx,
		`SELECT kind FROM job_errors WHERE job_id = $1 AND attempt = $2`, in.JobID, in.Attempt).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return kind == in.Outcome, nil
}

// endAttemptWithFailure records that an attempt failed and moves the job on. It is
// the retry-or-dead rule, shared by /report and (later) the reaper:
//
//	permanent                         -> dead   (T10)
//	any other kind, attempts left     -> queued, run_at = now + backoff (T9, T12)
//	any other kind, no attempts left  -> dead   (T11, T13)
//
// The caller holds the row lock and is inside a transaction.
func (s *Store) endAttemptWithFailure(ctx context.Context, tx pgx.Tx, id string, attempt, maxAttempts int,
	kind, message string) (state string, runAt *Time, err error) {

	_, err = tx.Exec(ctx,
		`INSERT INTO job_errors (job_id, attempt, kind, message) VALUES ($1, $2, $3, $4)`,
		id, attempt, kind, cleanMessage(message))
	if err != nil {
		return "", nil, err
	}

	if kind != OutcomePermanent && attempt < maxAttempts {
		s.rndMu.Lock()
		delay := retry.Delay(attempt, s.rnd)
		s.rndMu.Unlock()
		var t Time
		err = tx.QueryRow(ctx, `
			UPDATE jobs
			SET state = 'queued', run_at = now() + make_interval(secs => $2::float8),
			    lease_owner = NULL, lease_expires_at = NULL
			WHERE id = $1
			RETURNING run_at`, id, delay.Seconds()).Scan(&t)
		if err != nil {
			return "", nil, err
		}
		return "queued", &t, nil
	}

	_, err = tx.Exec(ctx, `
		UPDATE jobs
		SET state = 'dead', lease_owner = NULL, lease_expires_at = NULL, finished_at = now()
		WHERE id = $1`, id)
	if err != nil {
		return "", nil, err
	}
	return "dead", nil, nil
}

// cleanMessage keeps the first 1,000 characters (spec section 5). Postgres text
// cannot hold a NUL byte, and a handler's error text might, so it becomes U+FFFD.
func cleanMessage(m string) string {
	m = strings.ReplaceAll(m, "\x00", "\uFFFD")
	if !utf8.ValidString(m) {
		m = strings.ToValidUTF8(m, "\uFFFD")
	}
	if utf8.RuneCountInString(m) > MaxMessageChars {
		m = string([]rune(m)[:MaxMessageChars])
	}
	return m
}
