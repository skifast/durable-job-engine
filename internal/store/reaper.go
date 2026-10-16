package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// KindLeaseExpired is the error kind the reaper records when it takes a job back.
const KindLeaseExpired = "lease_expired"

// Both reaper jobs work in two steps. First a plain SELECT lists up to `limit`
// candidate ids. Then each candidate is handled in its own transaction, which
// locks the row and checks the condition again. The second check matters: between
// the SELECT and the lock a worker may have sent a heartbeat or a report, or
// another reaper may have got there first. In that case this reaper does nothing
// for that job. One transaction per job means one bad job cannot undo the others.

// ReapExpiredLeases takes back up to limit running jobs whose lease has run out
// (T12, T13) and returns how many it changed. Jobs beyond limit wait for the next tick.
func (s *Store) ReapExpiredLeases(ctx context.Context, limit int) (int, error) {
	ids, err := s.candidateIDs(ctx, `
		SELECT id FROM jobs
		WHERE state = 'running' AND lease_expires_at < now()
		ORDER BY lease_expires_at
		LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	reaped := 0
	for _, id := range ids {
		ok, err := s.reapOne(ctx, id)
		if err != nil {
			return reaped, err
		}
		if ok {
			reaped++
		}
	}
	return reaped, nil
}

// reapOne takes back one job, if it still needs it. It returns false when the job
// was already handled, renewed, or is locked by someone else right now.
func (s *Store) reapOne(ctx context.Context, id string) (bool, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	// SKIP LOCKED: if a report or another reaper holds the row, leave it. If that
	// transaction does not fix the job, the next tick will see it again.
	var attempt, maxAttempts int
	err = tx.QueryRow(ctx, `
		SELECT attempt, max_attempts FROM jobs
		WHERE id = $1 AND state = 'running' AND lease_expires_at < now()
		FOR UPDATE SKIP LOCKED`, id).Scan(&attempt, &maxAttempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	// The same retry-or-dead rule as /report uses, so the two paths cannot disagree.
	// The error entry's primary key (job_id, attempt) is the last guard against a
	// double requeue.
	if _, _, err := s.endAttemptWithFailure(ctx, tx, id, attempt, maxAttempts, KindLeaseExpired, "lease expired"); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// EscalateOverdueApprovals sets escalated_at on up to limit waiting_approval jobs
// whose deadline has passed (T5) and returns how many it changed. The job stays
// waiting_approval; nothing is rejected and nobody is notified.
func (s *Store) EscalateOverdueApprovals(ctx context.Context, limit int) (int, error) {
	ids, err := s.candidateIDs(ctx, `
		SELECT id FROM jobs
		WHERE state = 'waiting_approval' AND escalated_at IS NULL AND approval_deadline < now()
		ORDER BY approval_deadline
		LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	escalated := 0
	for _, id := range ids {
		// A single UPDATE is already one transaction. It waits for any lock on the
		// row, then checks the WHERE clause again, so an approve, a reject or another
		// reaper that finished first makes this change nothing (0 rows).
		tag, err := s.Pool.Exec(ctx, `
			UPDATE jobs SET escalated_at = now()
			WHERE id = $1 AND state = 'waiting_approval' AND escalated_at IS NULL AND approval_deadline < now()`, id)
		if err != nil {
			return escalated, err
		}
		escalated += int(tag.RowsAffected())
	}
	return escalated, nil
}

func (s *Store) candidateIDs(ctx context.Context, query string, limit int) ([]string, error) {
	rows, err := s.Pool.Query(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
