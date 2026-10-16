// Package api is the HTTP layer described in docs/api.md.
//
// Order of checks on every request: key (401), role (403), request format (400),
// then the work. The handlers call the store and never write SQL.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/skifast/durable-job-engine/internal/config"
	"github.com/skifast/durable-job-engine/internal/store"
	"github.com/skifast/durable-job-engine/internal/ui"
)

const (
	// MaxJSONBytes is the limit on a payload or a result: 64 KB of compact JSON (docs/api.md, decision 3).
	MaxJSONBytes = 64 * 1024
	// maxBodyBytes is how much of a request body is read at all. It is larger than
	// MaxJSONBytes because the body may be pretty-printed; the 64 KB rule is applied
	// to the compact form.
	maxBodyBytes = 1 << 20
)

// API holds what the handlers need.
type API struct {
	Store  *store.Store
	Config *config.Config
	Log    *slog.Logger
}

// Handler returns the router with the middleware around it.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.healthz)
	mux.HandleFunc("GET /whoami", a.auth(a.whoami, store.RoleClient, store.RoleWorker, store.RoleOperator))
	// The operator page: static files that need no key (ADR 015).
	mux.Handle("GET /ui/", ui.Handler())
	mux.HandleFunc("GET /ui", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ui/", http.StatusFound) })
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ui/", http.StatusFound) })
	mux.HandleFunc("POST /jobs", a.auth(a.submit, store.RoleClient))
	mux.HandleFunc("GET /jobs/{id}", a.auth(a.getJob, store.RoleClient, store.RoleOperator))
	mux.HandleFunc("GET /jobs", a.auth(a.listJobs, store.RoleClient, store.RoleOperator))
	mux.HandleFunc("POST /jobs/{id}/approve", a.auth(a.approve, store.RoleOperator))
	mux.HandleFunc("POST /jobs/{id}/reject", a.auth(a.reject, store.RoleOperator))
	mux.HandleFunc("POST /jobs/{id}/redrive", a.auth(a.redrive, store.RoleOperator))
	mux.HandleFunc("POST /claim", a.auth(a.claim, store.RoleWorker))
	mux.HandleFunc("POST /jobs/{id}/heartbeat", a.auth(a.heartbeat, store.RoleWorker))
	mux.HandleFunc("POST /jobs/{id}/report", a.auth(a.report, store.RoleWorker))
	// Anything else, as JSON in the same error shape.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "No such endpoint.", nil)
	})
	return a.recoverPanics(mux)
}

// ---------------------------------------------------------------------------
// Responses
// ---------------------------------------------------------------------------

type errorBody struct {
	Error errorDetail `json:"error"`
}
type errorDetail struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) // an error here means the client went away
}

func writeError(w http.ResponseWriter, status int, code, message string, details map[string]any) {
	writeJSON(w, status, errorBody{errorDetail{code, message, details}})
}

// fail answers an error from the store: a database that cannot be reached is 503,
// anything not expected is 500 (the details go to the log, not to the caller).
func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	if isUnavailable(err) {
		writeError(w, http.StatusServiceUnavailable, "database_unavailable",
			"The database could not be reached. Nothing was saved; retry.", nil)
		return
	}
	a.Log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
	writeError(w, http.StatusInternalServerError, "internal_error", "Internal error.", nil)
}

// isUnavailable is true when Postgres could not be reached or did not answer in time.
func isUnavailable(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var conn *pgconn.ConnectError
	if errors.As(err, &conn) {
		return true
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		// 08 connection exception, 53 out of resources, 57 operator intervention (shutdown, cancel)
		return strings.HasPrefix(pg.Code, "08") || strings.HasPrefix(pg.Code, "53") || strings.HasPrefix(pg.Code, "57")
	}
	return false
}

func (a *API) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				a.Log.Error("panic in handler", "method", r.Method, "path", r.URL.Path, "panic", v)
				writeError(w, http.StatusInternalServerError, "internal_error", "Internal error.", nil)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Authentication and roles (ADR 013)
// ---------------------------------------------------------------------------

type authedHandler func(w http.ResponseWriter, r *http.Request, caller store.Key)

// auth checks the key (401) and then the role (403) before the handler runs.
// Logs record the key's name and never the key.
func (a *API) auth(h authedHandler, allowed ...store.Role) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "A valid API key is required.", nil)
			return
		}
		caller, found, err := a.Store.LookupKey(r.Context(), token)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		if !found {
			writeError(w, http.StatusUnauthorized, "unauthorized", "A valid API key is required.", nil)
			return
		}
		for _, role := range allowed {
			if caller.Role == role {
				h(w, r, caller)
				return
			}
		}
		writeError(w, http.StatusForbidden, "forbidden", "This key's role may not make this call.", nil)
	}
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(h[len(prefix):]), true
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

// readBody reads the request body, up to maxBodyBytes. tooLarge is true if there was more.
func readBody(w http.ResponseWriter, r *http.Request) (body []byte, tooLarge bool, err error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	buf := new(strings.Builder)
	_, err = copyTo(buf, r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, true, nil
		}
		return nil, false, err
	}
	return []byte(buf.String()), false, nil
}

func badRequest(w http.ResponseWriter, message string) {
	writeError(w, http.StatusBadRequest, "invalid_request", message, nil)
}
