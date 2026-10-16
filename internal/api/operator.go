package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/skifast/durable-job-engine/internal/store"
)

const (
	defaultListLimit = 50
	maxListLimit     = 200
	maxReasonChars   = 1000
	maxRedriveLimit  = 100 // docs/api.md, decision 7
)

var listStates = map[string]bool{
	"queued": true, "waiting_approval": true, "running": true, "succeeded": true, "dead": true, "rejected": true,
}

// ---------------------------------------------------------------------------
// GET /jobs
// ---------------------------------------------------------------------------

func (a *API) listJobs(w http.ResponseWriter, r *http.Request, caller store.Key) {
	q := r.URL.Query()
	p := store.ListParams{Limit: defaultListLimit, Type: q.Get("type"), Cursor: q.Get("cursor")}

	// A client always gets its own jobs, whatever client_id says. An operator may narrow by client.
	if caller.Role == store.RoleClient {
		p.ClientID = caller.Name
	} else {
		p.ClientID = q.Get("client_id")
	}

	if v := q.Get("state"); v != "" {
		for _, s := range strings.Split(v, ",") {
			if !listStates[s] {
				badRequest(w, "state must be a comma-separated list of queued, waiting_approval, running, succeeded, dead, rejected.")
				return
			}
			p.States = append(p.States, s)
		}
	}
	switch q.Get("escalated") {
	case "", "false":
	case "true":
		if len(p.States) > 0 {
			badRequest(w, "escalated=true cannot be combined with state.")
			return
		}
		p.Escalated = true
	default:
		badRequest(w, "escalated must be true or false.")
		return
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxListLimit {
			badRequest(w, "limit must be a whole number from 1 to 200.")
			return
		}
		p.Limit = n
	}

	res, err := a.Store.List(r.Context(), p)
	switch {
	case err == nil:
		var next any // null on the last page
		if res.NextCursor != "" {
			next = res.NextCursor
		}
		writeJSON(w, http.StatusOK, map[string]any{"jobs": res.Jobs, "next_cursor": next})
	case errors.Is(err, store.ErrBadCursor):
		badRequest(w, "cursor is not valid. Use the next_cursor of the previous page.")
	default:
		a.fail(w, r, err)
	}
}

// ---------------------------------------------------------------------------
// POST /jobs/{id}/approve and /reject
// ---------------------------------------------------------------------------

func (a *API) approve(w http.ResponseWriter, r *http.Request, caller store.Key) {
	reason, ok := a.readReason(w, r, false)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !uuidPattern.MatchString(id) {
		notFound(w)
		return
	}
	job, err := a.Store.Approve(r.Context(), id, caller.Name, reason) // decided_by is the key's name
	a.answerOperator(w, r, job, err)
}

func (a *API) reject(w http.ResponseWriter, r *http.Request, caller store.Key) {
	reason, ok := a.readReason(w, r, true)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !uuidPattern.MatchString(id) {
		notFound(w)
		return
	}
	job, err := a.Store.Reject(r.Context(), id, caller.Name, reason)
	a.answerOperator(w, r, job, err)
}

// readReason reads {"reason": "..."}. For approve the body and the reason are optional;
// for reject the reason is required. It answers 400 itself.
func (a *API) readReason(w http.ResponseWriter, r *http.Request, required bool) (string, bool) {
	body, tooLarge, err := readBody(w, r)
	if err != nil || tooLarge {
		badRequest(w, "Could not read the request body.")
		return "", false
	}
	var in struct {
		Reason *string `json:"reason"`
	}
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &in); err != nil {
			badRequest(w, "The body must be a JSON object, for example {\"reason\": \"...\"}.")
			return "", false
		}
	}
	reason := ""
	if in.Reason != nil {
		reason = *in.Reason
	}
	if required && strings.TrimSpace(reason) == "" {
		badRequest(w, "A reject needs a reason.")
		return "", false
	}
	if utf8.RuneCountInString(reason) > maxReasonChars {
		badRequest(w, "reason is over 1,000 characters.")
		return "", false
	}
	return reason, true
}

// ---------------------------------------------------------------------------
// POST /jobs/{id}/redrive
// ---------------------------------------------------------------------------

func (a *API) redrive(w http.ResponseWriter, r *http.Request, caller store.Key) {
	body, tooLarge, err := readBody(w, r)
	if err != nil || tooLarge {
		badRequest(w, "Could not read the request body.")
		return
	}
	var in struct {
		MaxAttempts json.RawMessage `json:"max_attempts"`
	}
	if err := json.Unmarshal(body, &in); err != nil || len(in.MaxAttempts) == 0 {
		badRequest(w, "The body must be a JSON object with max_attempts, a whole number.")
		return
	}
	// Read the number's own text. Decoding into json.Number would also accept the string "5".
	text := string(in.MaxAttempts)
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		// "12.5", "1e3" and "\"5\"" are not whole numbers; a whole number too big for int64 is simply too high.
		if ne := (*strconv.NumError)(nil); errors.As(err, &ne) && ne.Err == strconv.ErrRange && !strings.HasPrefix(text, "-") {
			n = maxRedriveLimit + 1
		} else {
			badRequest(w, "max_attempts must be a whole number.")
			return
		}
	}
	if n > maxRedriveLimit {
		writeError(w, http.StatusBadRequest, "max_attempts_too_high", "max_attempts is over 100.", map[string]any{"limit": maxRedriveLimit})
		return
	}
	id := r.PathValue("id")
	if !uuidPattern.MatchString(id) {
		notFound(w)
		return
	}
	job, err := a.Store.Redrive(r.Context(), id, caller.Name, int(n))
	a.answerOperator(w, r, job, err)
}

// answerOperator writes the answer for approve, reject and redrive: the job, or the
// matching error from docs/api.md.
func (a *API) answerOperator(w http.ResponseWriter, r *http.Request, job *store.Job, err error) {
	var notWaiting *store.NotWaitingError
	var notDead *store.NotDeadError
	var tooLow *store.MaxAttemptsTooLowError
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, job)
	case errors.Is(err, store.ErrNotFound):
		notFound(w)
	case errors.Is(err, store.ErrBadContent):
		badRequest(w, "The reason holds content that cannot be stored (for example a \\u0000 character).")
	case errors.As(err, &notWaiting):
		details := map[string]any{"state": notWaiting.State}
		if notWaiting.DecidedBy != nil {
			details["decided_by"] = *notWaiting.DecidedBy
		}
		writeError(w, http.StatusConflict, "not_waiting_approval", "The job is not waiting for approval.", details)
	case errors.As(err, &notDead):
		writeError(w, http.StatusConflict, "not_dead", "Only a dead job can be redriven.", map[string]any{"state": notDead.State})
	case errors.As(err, &tooLow):
		writeError(w, http.StatusBadRequest, "max_attempts_too_low",
			"max_attempts must be greater than the number of attempts already made.", map[string]any{"attempt": tooLow.Attempt})
	default:
		a.fail(w, r, err)
	}
}

func notFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, "not_found", "No such job.", nil)
}
