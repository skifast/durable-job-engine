package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/skifast/durable-job-engine/internal/canonical"
	"github.com/skifast/durable-job-engine/internal/config"
	"github.com/skifast/durable-job-engine/internal/store"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ---------------------------------------------------------------------------
// GET /healthz
// ---------------------------------------------------------------------------

func (a *API) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.Store.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "database_unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ---------------------------------------------------------------------------
// GET /whoami
// ---------------------------------------------------------------------------

// whoami tells a caller which key it is using. The operator page shows it, because the key's
// name is what gets recorded as decided_by (ADR 015).
func (a *API) whoami(w http.ResponseWriter, r *http.Request, caller store.Key) {
	writeJSON(w, http.StatusOK, map[string]string{"name": caller.Name, "role": string(caller.Role)})
}

// ---------------------------------------------------------------------------
// POST /jobs
// ---------------------------------------------------------------------------

func (a *API) submit(w http.ResponseWriter, r *http.Request, caller store.Key) {
	// The Idempotency-Key header: 1 to 255 characters.
	key := r.Header.Get("Idempotency-Key")
	if n := utf8.RuneCountInString(key); n < 1 || n > 255 {
		badRequest(w, "The Idempotency-Key header is required and must be 1 to 255 characters.")
		return
	}

	body, tooLarge, err := readBody(w, r)
	if err != nil {
		badRequest(w, "Could not read the request body.")
		return
	}
	if tooLarge {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "The request body is too large.", nil)
		return
	}

	// The body is an object with "type" (a string) and "payload" (any JSON value, but present).
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		badRequest(w, "The body must be a JSON object with type and payload.")
		return
	}
	var jobType string
	if err := json.Unmarshal(fields["type"], &jobType); err != nil || jobType == "" {
		badRequest(w, "type is required and must be a string.")
		return
	}
	payload, hasPayload := fields["payload"]
	if !hasPayload {
		badRequest(w, "payload is required (it can be any JSON value, including null).")
		return
	}
	jt, known := a.Config.JobTypes[jobType]
	if !known {
		writeError(w, http.StatusBadRequest, "invalid_request", "Unknown job type.", map[string]any{"type": jobType})
		return
	}

	// Size is measured on the compact form.
	n, err := compactLen(payload)
	if err != nil {
		badRequest(w, "payload is not valid JSON.")
		return
	}
	if n > MaxJSONBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
			"The payload is over 64 KB of compact JSON.", map[string]any{"size": n, "limit": MaxJSONBytes})
		return
	}

	// payload_hash: SHA-256 of the canonical JSON (RFC 8785) of {type, payload}.
	typeJSON, _ := json.Marshal(jobType)
	canon, err := canonical.Marshal([]byte(`{"type":` + string(typeJSON) + `,"payload":` + string(payload) + `}`))
	if err != nil {
		badRequest(w, "payload cannot be used: "+err.Error()+".")
		return
	}
	sum := sha256.Sum256(canon)

	p := store.SubmitParams{
		ClientID:       caller.Name, // from the key, never from the request
		IdempotencyKey: key,
		Type:           jobType,
		Payload:        payload,
		PayloadHash:    hex.EncodeToString(sum[:]),
		MaxAttempts:    jt.MaxAttempts,
	}
	if jt.NeedsApproval {
		p.ApprovalWindow = jt.ApprovalWindow
	}

	job, created, err := a.Store.Submit(r.Context(), p)
	switch {
	case err == nil && created:
		w.Header().Set("Location", "/jobs/"+job.ID)
		writeJSON(w, http.StatusCreated, job)
	case err == nil:
		writeJSON(w, http.StatusOK, job)
	case errors.Is(err, store.ErrKeyReused):
		writeError(w, http.StatusUnprocessableEntity, "idempotency_key_reused",
			"This Idempotency-Key was already used with a different request.", nil)
	case errors.Is(err, store.ErrInProgress):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusConflict, "request_in_progress",
			"A request with this Idempotency-Key is still being saved. Retry shortly.", nil)
	case errors.Is(err, store.ErrBadContent):
		badRequest(w, "The payload holds content that cannot be stored (for example a \\u0000 character).")
	default:
		a.fail(w, r, err)
	}
}

// ---------------------------------------------------------------------------
// GET /jobs/{id}
// ---------------------------------------------------------------------------

func (a *API) getJob(w http.ResponseWriter, r *http.Request, caller store.Key) {
	id := r.PathValue("id")
	if !uuidPattern.MatchString(id) {
		writeError(w, http.StatusNotFound, "not_found", "No such job.", nil)
		return
	}
	owner := "" // an operator may read any job
	if caller.Role == store.RoleClient {
		owner = caller.Name // a client sees only its own; another's job is a 404
	}
	job, err := a.Store.GetJob(r.Context(), id, owner)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, job)
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "No such job.", nil)
	default:
		a.fail(w, r, err)
	}
}

// ---------------------------------------------------------------------------
// POST /claim
// ---------------------------------------------------------------------------

func (a *API) claim(w http.ResponseWriter, r *http.Request, caller store.Key) {
	item, err := a.Store.Claim(r.Context(), caller.Name) // lease_owner is the key's name
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if item == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// The time limit is a setting of the job type. A type removed from the file after
	// the job was submitted gets the default (docs/api.md).
	limit := config.DefaultHandlerTimeLimit
	if jt, ok := a.Config.JobTypes[item.Type]; ok {
		limit = jt.HandlerTimeLimit
	}
	item.HandlerTimeLimitMS = limit.Milliseconds()
	writeJSON(w, http.StatusOK, item)
}

// ---------------------------------------------------------------------------
// POST /jobs/{id}/heartbeat
// ---------------------------------------------------------------------------

func (a *API) heartbeat(w http.ResponseWriter, r *http.Request, caller store.Key) {
	id := r.PathValue("id")
	attempt, ok := a.readAttempt(w, r)
	if !ok {
		return
	}
	if !uuidPattern.MatchString(id) {
		writeError(w, http.StatusNotFound, "not_found", "No such job.", nil)
		return
	}
	expires, err := a.Store.Heartbeat(r.Context(), id, caller.Name, attempt)
	a.answerFenced(w, r, err, http.StatusOK, map[string]any{"lease_expires_at": expires}, attempt)
}

// readAttempt reads {"attempt": n} and answers 400 itself if it is not there.
func (a *API) readAttempt(w http.ResponseWriter, r *http.Request) (int, bool) {
	body, tooLarge, err := readBody(w, r)
	if err != nil || tooLarge {
		badRequest(w, "Could not read the request body.")
		return 0, false
	}
	var in struct {
		Attempt *int `json:"attempt"`
	}
	if err := json.Unmarshal(body, &in); err != nil || in.Attempt == nil || *in.Attempt < 1 {
		badRequest(w, "The body must be a JSON object with attempt, a whole number of 1 or more.")
		return 0, false
	}
	return *in.Attempt, true
}

// answerFenced writes the answer for a call that is checked against the fence:
// 404 for no such job, 409 lease_lost when the fence did not match.
func (a *API) answerFenced(w http.ResponseWriter, r *http.Request, err error, okStatus int, okBody any, attempt int) {
	var lost *store.LeaseLostError
	switch {
	case err == nil:
		writeJSON(w, okStatus, okBody)
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "No such job.", nil)
	case errors.As(err, &lost):
		writeError(w, http.StatusConflict, "lease_lost",
			"This worker no longer holds the lease for this attempt.", map[string]any{"state": lost.State})
	default:
		a.fail(w, r, err)
	}
}

// ---------------------------------------------------------------------------
// POST /jobs/{id}/report
// ---------------------------------------------------------------------------

func (a *API) report(w http.ResponseWriter, r *http.Request, caller store.Key) {
	id := r.PathValue("id")
	body, tooLarge, err := readBody(w, r)
	if err != nil {
		badRequest(w, "Could not read the request body.")
		return
	}
	if tooLarge {
		writeError(w, http.StatusRequestEntityTooLarge, "result_too_large", "The request body is too large.", nil)
		return
	}
	var in struct {
		Attempt *int            `json:"attempt"`
		Outcome string          `json:"outcome"`
		Result  json.RawMessage `json:"result"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(body, &in); err != nil || in.Attempt == nil || *in.Attempt < 1 {
		badRequest(w, "The body must be a JSON object with attempt (a whole number of 1 or more) and outcome.")
		return
	}
	switch in.Outcome {
	case store.OutcomeSuccess:
	case store.OutcomeRetryable, store.OutcomePermanent, store.OutcomeUnclassified:
		if in.Message == "" {
			badRequest(w, "A failure report needs a message.")
			return
		}
	default:
		badRequest(w, "outcome must be success, retryable, permanent or unclassified.")
		return
	}
	if !uuidPattern.MatchString(id) {
		writeError(w, http.StatusNotFound, "not_found", "No such job.", nil)
		return
	}

	rin := store.ReportInput{
		JobID: id, Worker: caller.Name, Attempt: *in.Attempt, Outcome: in.Outcome, Message: in.Message,
	}
	if in.Outcome == store.OutcomeSuccess && len(in.Result) > 0 {
		n, err := compactLen(in.Result)
		if err != nil {
			badRequest(w, "result is not valid JSON.")
			return
		}
		if n > MaxJSONBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "result_too_large",
				"The result is over 64 KB of compact JSON. Report a permanent failure instead.",
				map[string]any{"size": n, "limit": MaxJSONBytes})
			return
		}
		rin.Result = in.Result
	}

	res, err := a.Store.Report(r.Context(), rin)
	if errors.Is(err, store.ErrBadContent) {
		badRequest(w, "The result holds content that cannot be stored (for example a \\u0000 character).")
		return
	}
	a.answerFenced(w, r, err, http.StatusOK, res, *in.Attempt)
}
