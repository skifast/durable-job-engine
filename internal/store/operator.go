package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// List
// ---------------------------------------------------------------------------

// ListParams is a validated list call. The API has checked the states and the limit.
type ListParams struct {
	ClientID  string   // empty: all clients (operator)
	States    []string // empty: any state
	Escalated bool     // only waiting_approval jobs that have been escalated
	Type      string   // empty: any type
	Limit     int
	Cursor    string // "" for the first page
}

// ListResult is one page. NextCursor is empty on the last page.
type ListResult struct {
	Jobs       []Summary
	NextCursor string
}

// cursor marks the last job of a page. Paging by position (created_at, id), not by
// page number, means jobs submitted while someone pages do not shift the pages.
type cursor struct {
	CreatedAt time.Time `json:"c"`
	ID        string    `json:"i"`
}

// List returns jobs newest first. It asks for one more than Limit: if that extra job
// exists there is a next page, and the last job kept becomes the cursor.
func (s *Store) List(ctx context.Context, p ListParams) (*ListResult, error) {
	var cur *cursor
	if p.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(p.Cursor)
		cur = &cursor{}
		if err != nil || json.Unmarshal(raw, cur) != nil || !uuidLike(cur.ID) {
			return nil, ErrBadCursor
		}
	}

	// $n is NULL when a filter is not used, so one query text serves every combination.
	var clientID, jobType *string
	if p.ClientID != "" {
		clientID = &p.ClientID
	}
	if p.Type != "" {
		jobType = &p.Type
	}
	var curAt *time.Time
	var curID *string
	if cur != nil {
		curAt, curID = &cur.CreatedAt, &cur.ID
	}
	states := p.States
	if states == nil {
		states = []string{} // an empty list, not NULL: cardinality(NULL) would hide every row
	}
	if p.Escalated {
		states = []string{"waiting_approval"}
	}

	rows, err := s.Pool.Query(ctx, `
		SELECT j.id, j.type, j.client_id, j.state, j.attempt, j.max_attempts, j.run_at,
		       j.approval_deadline, j.escalated_at, j.created_at, j.finished_at,
		       e.attempt, e.kind, e.message
		FROM jobs j
		LEFT JOIN LATERAL (
		    SELECT attempt, kind, message FROM job_errors
		    WHERE job_id = j.id ORDER BY attempt DESC LIMIT 1
		) e ON true
		WHERE ($1::text IS NULL OR j.client_id = $1)
		  AND (cardinality($2::text[]) = 0 OR j.state = ANY($2))
		  AND (NOT $3 OR j.escalated_at IS NOT NULL)
		  AND ($4::text IS NULL OR j.type = $4)
		  AND ($5::timestamptz IS NULL OR (j.created_at, j.id) < ($5, $6::uuid))
		ORDER BY j.created_at DESC, j.id DESC
		LIMIT $7`,
		clientID, states, p.Escalated, jobType, curAt, curID, p.Limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := &ListResult{Jobs: []Summary{}}
	for rows.Next() {
		var j Summary
		var eAttempt *int
		var eKind, eMsg *string
		if err := rows.Scan(&j.ID, &j.Type, &j.ClientID, &j.State, &j.Attempt, &j.MaxAttempts, &j.RunAt,
			&j.ApprovalDeadline, &j.EscalatedAt, &j.CreatedAt, &j.FinishedAt, &eAttempt, &eKind, &eMsg); err != nil {
			return nil, err
		}
		if eAttempt != nil {
			j.LastError = &LastError{Attempt: *eAttempt, Kind: *eKind, Message: *eMsg}
		}
		res.Jobs = append(res.Jobs, j)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(res.Jobs) > p.Limit {
		res.Jobs = res.Jobs[:p.Limit]
		last := res.Jobs[p.Limit-1]
		raw, _ := json.Marshal(cursor{CreatedAt: last.CreatedAt.Std(), ID: last.ID})
		res.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return res, nil
}

// uuidLike keeps a hand-made cursor from reaching the ::uuid cast and failing there.
func uuidLike(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'):
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Approve (T3) and reject (T4)
// ---------------------------------------------------------------------------

// Approve queues a waiting_approval job, also after it was escalated. reason may be empty.
func (s *Store) Approve(ctx context.Context, id, operator, reason string) (*Job, error) {
	return s.decide(ctx, id, `
		UPDATE jobs
		SET state = 'queued', run_at = now(), decided_by = $2, decided_at = now(), decision_reason = $3
		WHERE id = $1 AND state = 'waiting_approval'`, operator, nullIfEmpty(reason))
}

// Reject ends a waiting_approval job for good. The reason is required (the database checks too).
func (s *Store) Reject(ctx context.Context, id, operator, reason string) (*Job, error) {
	return s.decide(ctx, id, `
		UPDATE jobs
		SET state = 'rejected', decided_by = $2, decided_at = now(), decision_reason = $3, finished_at = now()
		WHERE id = $1 AND state = 'waiting_approval'`, operator, reason)
}

// decide runs an approve or reject UPDATE. The UPDATE takes the row lock and checks
// state = 'waiting_approval' after any wait, so of two operators acting at once one
// changes the row and the other changes nothing and gets a NotWaitingError.
func (s *Store) decide(ctx context.Context, id, update, operator string, reason any) (*Job, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, update, id, operator, reason)
	if err != nil {
		return nil, mapContentError(err)
	}
	if tag.RowsAffected() == 0 {
		var state string
		var decidedBy *string
		err := tx.QueryRow(ctx, `SELECT state, decided_by FROM jobs WHERE id = $1`, id).Scan(&state, &decidedBy)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		return nil, &NotWaitingError{State: state, DecidedBy: decidedBy}
	}
	return commitAndLoad(ctx, tx, id)
}

// ---------------------------------------------------------------------------
// Redrive (T14)
// ---------------------------------------------------------------------------

// Redrive puts a dead job back in the queue with a new max_attempts. attempt is not
// reset, so max_attempts must be above it (ADR 009); the database enforces that too.
func (s *Store) Redrive(ctx context.Context, id, operator string, maxAttempts int) (*Job, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// Lock the row, then look at it, so a second redrive waits and then sees "queued".
	var state string
	var attempt int
	err = tx.QueryRow(ctx, `SELECT state, attempt FROM jobs WHERE id = $1 FOR UPDATE`, id).Scan(&state, &attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if state != "dead" {
		return nil, &NotDeadError{State: state}
	}
	if maxAttempts <= attempt {
		return nil, &MaxAttemptsTooLowError{Attempt: attempt}
	}
	_, err = tx.Exec(ctx, `
		UPDATE jobs
		SET state = 'queued', max_attempts = $2, run_at = now(), finished_at = NULL,
		    redriven_by = $3, redriven_at = now()
		WHERE id = $1`, id, maxAttempts, operator)
	if err != nil {
		return nil, err
	}
	return commitAndLoad(ctx, tx, id)
}

// commitAndLoad reads the job inside the transaction (so the answer is exactly what was
// written), then commits.
func commitAndLoad(ctx context.Context, tx pgx.Tx, id string) (*Job, error) {
	job, err := loadJob(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return job, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
