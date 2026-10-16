package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/skifast/durable-job-engine/internal/api"
	"github.com/skifast/durable-job-engine/internal/config"
	"github.com/skifast/durable-job-engine/internal/store"
	"github.com/skifast/durable-job-engine/testutil"
)

// ---------------------------------------------------------------------------
// Test setup
// ---------------------------------------------------------------------------

// testLog sends the engine's log lines to the test output, so a 500 in a test shows its cause.
type testLog struct{ t *testing.T }

func (l testLog) Write(p []byte) (int, error) {
	l.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

type env struct {
	t     *testing.T
	pool  *pgxpool.Pool
	store *store.Store
	srv   *httptest.Server
}

// Keys created for every test: name -> role. The token for a name is "token-<name>".
var testKeys = map[string]store.Role{
	"alice": store.RoleClient, "bob": store.RoleClient,
	"w1": store.RoleWorker, "w2": store.RoleWorker,
	"op1": store.RoleOperator,
}

func token(name string) string { return "token-" + name }

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := testutil.NewDB(t)
	for name, role := range testKeys {
		_, err := pool.Exec(context.Background(),
			`INSERT INTO api_keys (name, role, key_hash) VALUES ($1, $2, $3)`, name, string(role), store.HashKey(token(name)))
		if err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := config.Parse([]byte(`{"job_types": {
		"plain":     {},
		"limited":   {"handler_time_limit": "1500ms"},
		"two_tries": {"max_attempts": 2},
		"one_try":   {"max_attempts": 1},
		"gated":     {"needs_approval": true, "approval_window": "1h"}
	}}`))
	if err != nil {
		t.Fatal(err)
	}
	st := store.New(pool)
	a := &api.API{Store: st, Config: cfg, Log: slog.New(slog.NewTextHandler(testLog{t}, nil))}
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	// Whatever a test did, the database must still satisfy every invariant at the end.
	t.Cleanup(func() { testutil.CheckInvariants(t, pool) })
	return &env{t: t, pool: pool, store: st, srv: srv}
}

type resp struct {
	status int
	header http.Header
	body   []byte
}

func (r resp) json() map[string]any {
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); err != nil {
		panic("response is not a JSON object: " + string(r.body))
	}
	return m
}

// errCode returns error.code from an error response.
func (r resp) errCode() string {
	e, _ := r.json()["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

// call sends a request. who is a key name ("alice"), "" for no key, or "nokey-xyz" style raw tokens
// when prefixed with "raw:". body may be a string, []byte, nil, or anything json.Marshal accepts.
func (e *env) call(method, path, who string, headers map[string]string, body any) resp {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			e.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	switch {
	case who == "":
	case strings.HasPrefix(who, "raw:"):
		req.Header.Set("Authorization", "Bearer "+strings.TrimPrefix(who, "raw:"))
	default:
		req.Header.Set("Authorization", "Bearer "+token(who))
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{res.StatusCode, res.Header, b}
}

func (e *env) submit(who, key, typ string, payload any) resp {
	e.t.Helper()
	return e.call("POST", "/jobs", who, map[string]string{"Idempotency-Key": key},
		map[string]any{"type": typ, "payload": payload})
}

// mustSubmit submits a job and returns its id.
func (e *env) mustSubmit(who, key, typ string) string {
	e.t.Helper()
	r := e.submit(who, key, typ, map[string]any{"n": 1})
	if r.status != 201 {
		e.t.Fatalf("submit: status %d: %s", r.status, r.body)
	}
	return r.json()["id"].(string)
}

// mustClaim claims as a worker and returns the work item.
func (e *env) mustClaim(who string) map[string]any {
	e.t.Helper()
	r := e.call("POST", "/claim", who, nil, nil)
	if r.status != 200 {
		e.t.Fatalf("claim: status %d: %s", r.status, r.body)
	}
	return r.json()
}

func (e *env) report(who, id string, attempt int, outcome string, extra map[string]any) resp {
	e.t.Helper()
	body := map[string]any{"attempt": attempt, "outcome": outcome}
	for k, v := range extra {
		body[k] = v
	}
	return e.call("POST", "/jobs/"+id+"/report", who, nil, body)
}

// makeDue moves a queued job's run_at into the past, so it can be claimed now. The
// schema's trigger refuses this change (queued -> queued is not a legal transition), so
// the test turns triggers off for this one transaction (session_replication_role = replica).
func (e *env) makeDue(id string) {
	e.t.Helper()
	tx, err := e.pool.Begin(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(context.Background(), "SET LOCAL session_replication_role = replica"); err != nil {
		e.t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `UPDATE jobs SET run_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		e.t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) job(id string) map[string]any {
	e.t.Helper()
	r := e.call("GET", "/jobs/"+id, "op1", nil, nil)
	if r.status != 200 {
		e.t.Fatalf("get job: status %d: %s", r.status, r.body)
	}
	return r.json()
}

func want(t *testing.T, what string, got, wantv any) {
	t.Helper()
	if got != wantv {
		t.Errorf("%s: got %v, want %v", what, got, wantv)
	}
}

// ---------------------------------------------------------------------------
// Authentication and roles
// ---------------------------------------------------------------------------

func TestEveryEndpointNeedsAKeyAndTheRightRole(t *testing.T) {
	e := newEnv(t)
	id := e.mustSubmit("alice", "k-auth", "plain")
	if err := func() error {
		_, err := e.pool.Exec(context.Background(), `INSERT INTO api_keys (name, role, key_hash) VALUES ('old-w', 'worker', $1)`, store.HashKey("old"))
		return err
	}(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE api_keys SET revoked_at = now() WHERE name = 'old-w'`); err != nil {
		t.Fatal(err)
	}

	type ep struct {
		method, path string
		allowed      []string // key names that may call it
		body         any
	}
	eps := []ep{
		{"POST", "/jobs", []string{"alice"}, map[string]any{"type": "plain", "payload": 1}},
		{"GET", "/jobs/" + id, []string{"alice", "op1"}, nil},
		{"GET", "/whoami", []string{"alice", "w1", "op1"}, nil},
		{"GET", "/jobs", []string{"alice", "op1"}, nil},
		{"POST", "/jobs/" + id + "/approve", []string{"op1"}, nil},
		{"POST", "/jobs/" + id + "/reject", []string{"op1"}, map[string]any{"reason": "x"}},
		{"POST", "/jobs/" + id + "/redrive", []string{"op1"}, map[string]any{"max_attempts": 5}},
		{"POST", "/claim", []string{"w1"}, nil},
		{"POST", "/jobs/" + id + "/heartbeat", []string{"w1"}, map[string]any{"attempt": 1}},
		{"POST", "/jobs/" + id + "/report", []string{"w1"}, map[string]any{"attempt": 1, "outcome": "success"}},
	}
	for _, p := range eps {
		name := p.method + " " + p.path
		// No key, an unknown key, a revoked key: 401.
		for _, who := range []string{"", "raw:not-a-key", "raw:old"} {
			r := e.call(p.method, p.path, who, map[string]string{"Idempotency-Key": "x"}, p.body)
			if r.status != 401 || r.errCode() != "unauthorized" {
				t.Errorf("%s with key %q: got %d %s, want 401 unauthorized", name, who, r.status, r.body)
			}
		}
		// A valid key with the wrong role: 403. (One key from each role that is not allowed.)
		for _, who := range []string{"alice", "w1", "op1"} {
			allowed := false
			for _, a := range p.allowed {
				allowed = allowed || a == who
			}
			if allowed {
				continue
			}
			r := e.call(p.method, p.path, who, map[string]string{"Idempotency-Key": "x"}, p.body)
			if r.status != 403 || r.errCode() != "forbidden" {
				t.Errorf("%s as %s: got %d %s, want 403 forbidden", name, who, r.status, r.body)
			}
		}
	}
	// Health needs no key.
	if r := e.call("GET", "/healthz", "", nil, nil); r.status != 200 {
		t.Errorf("healthz: got %d", r.status)
	}
}

// A revoke takes effect on the next request (ADR 013: no caching of keys).
func TestRevokeTakesEffectAtOnce(t *testing.T) {
	e := newEnv(t)
	if r := e.call("POST", "/claim", "w2", nil, nil); r.status != 204 {
		t.Fatalf("before revoke: %d", r.status)
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE api_keys SET revoked_at = now() WHERE name = 'w2'`); err != nil {
		t.Fatal(err)
	}
	if r := e.call("POST", "/claim", "w2", nil, nil); r.status != 401 {
		t.Errorf("after revoke: got %d, want 401", r.status)
	}
}

// ---------------------------------------------------------------------------
// Submit
// ---------------------------------------------------------------------------

func TestSubmitCreatesThenRepeats(t *testing.T) {
	e := newEnv(t)
	r := e.submit("alice", "k1", "plain", map[string]any{"invoice": 1234})
	want(t, "status", r.status, 201)
	j := r.json()
	want(t, "state", j["state"], "queued")
	want(t, "client_id comes from the key", j["client_id"], "alice")
	want(t, "attempt", j["attempt"], float64(0))
	want(t, "max_attempts", j["max_attempts"], float64(8))
	want(t, "Location", r.header.Get("Location"), "/jobs/"+j["id"].(string))
	if _, ok := j["errors"].([]any); !ok {
		t.Errorf("errors should be an empty list, got %v", j["errors"])
	}

	// The same key and request again: 200, the same job.
	r2 := e.submit("alice", "k1", "plain", map[string]any{"invoice": 1234})
	want(t, "repeat status", r2.status, 200)
	want(t, "repeat id", r2.json()["id"], j["id"])

	// Same data, keys in another order, extra spaces: still the same request.
	r3 := e.call("POST", "/jobs", "alice", map[string]string{"Idempotency-Key": "k-order"},
		`{"type":"plain","payload":{"b":1,"a":[1, 2]}}`)
	want(t, "first of order test", r3.status, 201)
	r4 := e.call("POST", "/jobs", "alice", map[string]string{"Idempotency-Key": "k-order"},
		`{ "payload": { "a": [1,2], "b": 1.0 }, "type": "plain" }`)
	want(t, "reordered repeat", r4.status, 200)

	// Same key, different payload or type: 422, nothing changes.
	r5 := e.submit("alice", "k1", "plain", map[string]any{"invoice": 9999})
	want(t, "different payload", r5.status, 422)
	want(t, "code", r5.errCode(), "idempotency_key_reused")
	r6 := e.submit("alice", "k1", "two_tries", map[string]any{"invoice": 1234})
	want(t, "different type", r6.status, 422)

	// A repeat after the job has moved on returns the job as it is now.
	w := e.mustClaim("w1")
	rr := e.report("w1", w["id"].(string), 1, "success", nil)
	want(t, "report", rr.status, 200)
	r7 := e.submit("alice", "k1", "plain", map[string]any{"invoice": 1234})
	want(t, "repeat after success", r7.status, 200)
	want(t, "state is current", r7.json()["state"], "succeeded")
}

// Two clients may use the same key; each has its own jobs.
func TestSameKeyDifferentClients(t *testing.T) {
	e := newEnv(t)
	a := e.submit("alice", "shared", "plain", 1)
	b := e.submit("bob", "shared", "plain", 1)
	want(t, "alice", a.status, 201)
	want(t, "bob", b.status, 201)
	if a.json()["id"] == b.json()["id"] {
		t.Error("two clients got the same job")
	}
}

// client_id is the key's name, whatever the request says.
func TestClientIDCannotBeSpoofed(t *testing.T) {
	e := newEnv(t)
	r := e.call("POST", "/jobs", "alice",
		map[string]string{"Idempotency-Key": "spoof", "X-Client-Id": "bob"},
		`{"type":"plain","payload":null,"client_id":"bob"}`)
	want(t, "status", r.status, 201)
	want(t, "client_id", r.json()["client_id"], "alice")
}

func TestSubmitBadRequests(t *testing.T) {
	e := newEnv(t)
	long := strings.Repeat("k", 256)
	cases := []struct {
		name    string
		headers map[string]string
		body    any
		status  int
		code    string
	}{
		{"missing key header", nil, `{"type":"plain","payload":1}`, 400, "invalid_request"},
		{"empty key header", map[string]string{"Idempotency-Key": ""}, `{"type":"plain","payload":1}`, 400, "invalid_request"},
		{"key of 256 characters", map[string]string{"Idempotency-Key": long}, `{"type":"plain","payload":1}`, 400, "invalid_request"},
		{"not json", map[string]string{"Idempotency-Key": "a"}, `{type:`, 400, "invalid_request"},
		{"body is an array", map[string]string{"Idempotency-Key": "a"}, `[1]`, 400, "invalid_request"},
		{"no type", map[string]string{"Idempotency-Key": "a"}, `{"payload":1}`, 400, "invalid_request"},
		{"type is a number", map[string]string{"Idempotency-Key": "a"}, `{"type":5,"payload":1}`, 400, "invalid_request"},
		{"no payload", map[string]string{"Idempotency-Key": "a"}, `{"type":"plain"}`, 400, "invalid_request"},
		{"unknown job type", map[string]string{"Idempotency-Key": "a"}, `{"type":"nope","payload":1}`, 400, "invalid_request"},
		{"NUL character in payload", map[string]string{"Idempotency-Key": "a"}, `{"type":"plain","payload":{"x":"a\u0000b"}}`, 400, "invalid_request"},
		{"number too big for a double", map[string]string{"Idempotency-Key": "a"}, `{"type":"plain","payload":1e999}`, 400, "invalid_request"},
	}
	for _, c := range cases {
		r := e.call("POST", "/jobs", "alice", c.headers, c.body)
		if r.status != c.status || r.errCode() != c.code {
			t.Errorf("%s: got %d %s, want %d %s", c.name, r.status, r.body, c.status, c.code)
		}
	}
	// A key of exactly 255 characters is fine.
	r := e.call("POST", "/jobs", "alice", map[string]string{"Idempotency-Key": strings.Repeat("k", 255)}, `{"type":"plain","payload":1}`)
	want(t, "255-character key", r.status, 201)
	// Nothing was stored by the refused requests, so a refused key is still free.
	var n int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM jobs`).Scan(&n)
	want(t, "jobs stored", n, 1)
	// payload may be null, a string, a number.
	for i, p := range []string{`null`, `"text"`, `0`, `[]`} {
		r := e.call("POST", "/jobs", "alice", map[string]string{"Idempotency-Key": "p" + string(rune('a'+i))},
			`{"type":"plain","payload":`+p+`}`)
		if r.status != 201 {
			t.Errorf("payload %s: got %d %s", p, r.status, r.body)
		}
	}
}

// The 64 KB limit is measured on compact JSON (docs/api.md, decision 3).
func TestPayloadSizeLimit(t *testing.T) {
	e := newEnv(t)
	// A string payload: the JSON text is the string plus two quotes.
	atLimit := `"` + strings.Repeat("a", 64*1024-2) + `"`
	over := `"` + strings.Repeat("a", 64*1024-1) + `"`
	h := map[string]string{"Idempotency-Key": "big"}
	r := e.call("POST", "/jobs", "alice", h, `{"type":"plain","payload":`+atLimit+`}`)
	want(t, "payload of exactly 64 KB", r.status, 201)
	h["Idempotency-Key"] = "bigger"
	r = e.call("POST", "/jobs", "alice", h, `{"type":"plain","payload":`+over+`}`)
	want(t, "one byte over", r.status, 413)
	want(t, "code", r.errCode(), "payload_too_large")

	// A request body over 64 KB is fine if the payload is under 64 KB once the spaces are removed.
	spaced := `[` + strings.Repeat("1 ,\n\t", 20000) + `1]` // about 100 KB of text, 40 KB compact
	h["Idempotency-Key"] = "spaced"
	r = e.call("POST", "/jobs", "alice", h, `{"type":"plain","payload":`+spaced+`}`)
	want(t, "spaced payload under the compact limit", r.status, 201)
}

func TestSubmitNeedingApprovalStartsWaiting(t *testing.T) {
	e := newEnv(t)
	r := e.submit("alice", "g1", "gated", 1)
	want(t, "status", r.status, 201)
	j := r.json()
	want(t, "state", j["state"], "waiting_approval")
	if j["approval_deadline"] == nil {
		t.Error("approval_deadline should be set")
	}
	// A waiting job is not handed to a worker.
	if c := e.call("POST", "/claim", "w1", nil, nil); c.status != 204 {
		t.Errorf("claim of a waiting job: got %d, want 204", c.status)
	}
}

// While the first request with a key is still open, a second one waits up to 2 s
// and then gets 409 (ADR 005).
func TestDuplicateWhileFirstIsOpen(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// An open insert that has not committed: it holds the unique-index entry for (alice, held).
	_, err = tx.Exec(ctx, `INSERT INTO jobs (type, client_id, idempotency_key, payload_hash, payload, state)
	                       VALUES ('plain', 'alice', 'held', repeat('a', 64), '1', 'queued')`)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	r := e.submit("alice", "held", "plain", 1)
	took := time.Since(start)
	want(t, "status", r.status, 409)
	want(t, "code", r.errCode(), "request_in_progress")
	want(t, "Retry-After", r.header.Get("Retry-After"), "1")
	if took < 1900*time.Millisecond || took > 4*time.Second {
		t.Errorf("waited %v, want about 2 s", took)
	}
	tx.Rollback(ctx)
	// The key is free again once the first request rolled back.
	r = e.submit("alice", "held", "plain", 1)
	want(t, "after rollback", r.status, 201)
}

// Ten requests with one key at the same moment: one job, one 201, nine 200.
func TestConcurrentDuplicateSubmits(t *testing.T) {
	e := newEnv(t)
	var wg sync.WaitGroup
	statuses := make([]int, 10)
	ids := make([]string, 10)
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := e.call("POST", "/jobs", "alice", map[string]string{"Idempotency-Key": "race"},
				`{"type":"plain","payload":{"same":true}}`)
			statuses[i] = r.status
			if r.status == 200 || r.status == 201 {
				ids[i] = r.json()["id"].(string)
			}
		}(i)
	}
	wg.Wait()
	created := 0
	for i, s := range statuses {
		switch s {
		case 201:
			created++
		case 200:
		default:
			t.Errorf("request %d: status %d", i, s)
		}
		if ids[i] != ids[0] {
			t.Errorf("request %d got job %s, request 0 got %s", i, ids[i], ids[0])
		}
	}
	want(t, "number of 201 responses", created, 1)
	var n int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM jobs`).Scan(&n)
	want(t, "jobs stored", n, 1)
}

// ---------------------------------------------------------------------------
// Read
// ---------------------------------------------------------------------------

func TestGetJobVisibility(t *testing.T) {
	e := newEnv(t)
	id := e.mustSubmit("alice", "g", "plain")
	want(t, "owner", e.call("GET", "/jobs/"+id, "alice", nil, nil).status, 200)
	want(t, "operator", e.call("GET", "/jobs/"+id, "op1", nil, nil).status, 200)
	// Another client's job looks like a job that does not exist.
	other := e.call("GET", "/jobs/"+id, "bob", nil, nil)
	want(t, "other client", other.status, 404)
	want(t, "other client code", other.errCode(), "not_found")
	missing := e.call("GET", "/jobs/6f1c0e5a-6a53-4d6e-9d57-0a2b7f2f6b11", "bob", nil, nil)
	if string(missing.body) != string(other.body) {
		t.Errorf("a missing job and another client's job should look the same:\n%s\n%s", missing.body, other.body)
	}
	want(t, "not a uuid", e.call("GET", "/jobs/nope", "op1", nil, nil).status, 404)
	want(t, "unknown endpoint", e.call("GET", "/nowhere", "op1", nil, nil).status, 404)
}

// ---------------------------------------------------------------------------
// Claim
// ---------------------------------------------------------------------------

func TestClaim(t *testing.T) {
	e := newEnv(t)
	want(t, "empty queue", e.call("POST", "/claim", "w1", nil, nil).status, 204)

	// Jobs are claimed in the order they became due. Six jobs make it very unlikely that
	// a wrong order matches by chance (ids are random).
	var order []string
	for i := 0; i < 6; i++ {
		order = append(order, e.mustSubmit("alice", "order"+string(rune('a'+i)), "plain"))
	}
	for i, id := range order {
		w := e.mustClaim("w2")
		if w["id"] != id {
			t.Fatalf("claim %d returned job %v, want %v (oldest first)", i+1, w["id"], id)
		}
		e.report("w2", id, 1, "success", nil)
	}

	first := e.mustSubmit("alice", "c1", "plain")
	second := e.mustSubmit("alice", "c2", "plain")
	w := e.mustClaim("w1")
	want(t, "oldest first", w["id"], first)
	want(t, "attempt", w["attempt"], float64(1))
	want(t, "type", w["type"], "plain")
	if w["lease_expires_at"] == nil || w["payload"] == nil {
		t.Errorf("work item incomplete: %v", w)
	}
	j := e.job(first)
	want(t, "state", j["state"], "running")
	want(t, "lease_owner is the key's name", j["lease_owner"], "w1")

	w2 := e.mustClaim("w2")
	want(t, "second worker gets the next job", w2["id"], second)
	want(t, "nothing left", e.call("POST", "/claim", "w1", nil, nil).status, 204)
}

// A job whose run_at is in the future is not claimable.
// The work item carries the job type's handler time limit (ADR 014).
func TestClaimCarriesTimeLimit(t *testing.T) {
	e := newEnv(t)
	e.mustSubmit("alice", "lim1", "limited")
	want(t, "configured limit", e.mustClaim("w1")["handler_time_limit_ms"], float64(1500))
	e.mustSubmit("alice", "lim2", "plain")
	want(t, "default limit", e.mustClaim("w1")["handler_time_limit_ms"], float64(60000))

	// A job whose type is no longer in the file gets the default. The store does not check
	// types (the API does at submit), so a direct submit stands in for a changed file.
	if _, _, err := e.store.Submit(context.Background(), store.SubmitParams{
		ClientID: "alice", IdempotencyKey: "gone", Type: "removed_type", Payload: []byte(`{}`),
		PayloadHash: store.HashKey("gone"), MaxAttempts: 3,
	}); err != nil {
		t.Fatal(err)
	}
	want(t, "removed type", e.mustClaim("w1")["handler_time_limit_ms"], float64(60000))
}

func TestClaimSkipsFutureJobs(t *testing.T) {
	e := newEnv(t)
	id := e.mustSubmit("alice", "f", "plain")
	w := e.mustClaim("w1")
	rr := e.report("w1", id, int(w["attempt"].(float64)), "retryable", map[string]any{"message": "try later"})
	want(t, "report", rr.status, 200)
	want(t, "waiting for run_at", e.call("POST", "/claim", "w1", nil, nil).status, 204)
	e.makeDue(id)
	w = e.mustClaim("w2")
	want(t, "second attempt", w["attempt"], float64(2))
}

// Many workers claiming at once: every job goes to exactly one of them.
func TestConcurrentClaims(t *testing.T) {
	e := newEnv(t)
	const jobs = 40
	for i := 0; i < jobs; i++ {
		e.mustSubmit("alice", "cc"+string(rune('A'+i/26))+string(rune('a'+i%26)), "plain")
	}
	var mu sync.Mutex
	got := map[string]int{}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			who := "w1"
			if g%2 == 1 {
				who = "w2"
			}
			for {
				r := e.call("POST", "/claim", who, nil, nil)
				if r.status == 204 {
					return
				}
				if r.status != 200 {
					t.Errorf("claim: %d %s", r.status, r.body)
					return
				}
				mu.Lock()
				got[r.json()["id"].(string)]++
				mu.Unlock()
			}
		}(g)
	}
	wg.Wait()
	want(t, "distinct jobs claimed", len(got), jobs)
	for id, n := range got {
		if n != 1 {
			t.Errorf("job %s was claimed %d times", id, n)
		}
	}
}

// ---------------------------------------------------------------------------
// Heartbeat
// ---------------------------------------------------------------------------

func TestHeartbeat(t *testing.T) {
	e := newEnv(t)
	id := e.mustSubmit("alice", "h", "plain")
	w := e.mustClaim("w1")
	before := e.job(id)["lease_expires_at"].(string)

	time.Sleep(20 * time.Millisecond)
	r := e.call("POST", "/jobs/"+id+"/heartbeat", "w1", nil, map[string]any{"attempt": 1})
	want(t, "status", r.status, 200)
	after := r.json()["lease_expires_at"].(string)
	if after <= before {
		t.Errorf("lease not extended: %s then %s", before, after)
	}
	_ = w

	// Wrong worker, wrong attempt: 409 lease_lost, nothing changes.
	for name, c := range map[string]struct {
		who     string
		attempt int
	}{"wrong worker": {"w2", 1}, "wrong attempt": {"w1", 2}} {
		r := e.call("POST", "/jobs/"+id+"/heartbeat", c.who, nil, map[string]any{"attempt": c.attempt})
		if r.status != 409 || r.errCode() != "lease_lost" {
			t.Errorf("%s: got %d %s", name, r.status, r.body)
		}
	}
	want(t, "lease unchanged by refused heartbeats", e.job(id)["lease_expires_at"], after)

	// 400 for a bad body, 404 for no such job.
	want(t, "no attempt", e.call("POST", "/jobs/"+id+"/heartbeat", "w1", nil, `{}`).status, 400)
	want(t, "attempt 0", e.call("POST", "/jobs/"+id+"/heartbeat", "w1", nil, `{"attempt":0}`).status, 400)
	want(t, "attempt as text", e.call("POST", "/jobs/"+id+"/heartbeat", "w1", nil, `{"attempt":"1"}`).status, 400)
	want(t, "no such job", e.call("POST", "/jobs/6f1c0e5a-6a53-4d6e-9d57-0a2b7f2f6b11/heartbeat", "w1", nil, map[string]any{"attempt": 1}).status, 404)

	// After the job has finished, a heartbeat is refused and says where the job is.
	e.report("w1", id, 1, "success", nil)
	r = e.call("POST", "/jobs/"+id+"/heartbeat", "w1", nil, map[string]any{"attempt": 1})
	want(t, "heartbeat after success", r.status, 409)
	details := r.json()["error"].(map[string]any)["details"].(map[string]any)
	want(t, "details.state", details["state"], "succeeded")
}

// ---------------------------------------------------------------------------
// Report
// ---------------------------------------------------------------------------

func TestReportSuccess(t *testing.T) {
	e := newEnv(t)
	id := e.mustSubmit("alice", "s", "plain")
	e.mustClaim("w1")
	r := e.report("w1", id, 1, "success", map[string]any{"result": map[string]any{"sent": true}})
	want(t, "status", r.status, 200)
	want(t, "state", r.json()["state"], "succeeded")
	want(t, "replayed", r.json()["replayed"], false)
	j := e.job(id)
	want(t, "state", j["state"], "succeeded")
	want(t, "result", j["result"].(map[string]any)["sent"], true)
	want(t, "lease cleared", j["lease_owner"], nil)
	if j["finished_at"] == nil {
		t.Error("finished_at not set")
	}

	// A success with no result stores {}.
	id2 := e.mustSubmit("alice", "s2", "plain")
	e.mustClaim("w1")
	e.report("w1", id2, 1, "success", nil)
	want(t, "default result", len(e.job(id2)["result"].(map[string]any)), 0)

	// The result can be any JSON value, for example a number or a string.
	id3 := e.mustSubmit("alice", "s3", "plain")
	e.mustClaim("w1")
	e.report("w1", id3, 1, "success", map[string]any{"result": "done"})
	want(t, "string result", e.job(id3)["result"], "done")
}

func TestReportRetryableThenDead(t *testing.T) {
	e := newEnv(t)
	id := e.mustSubmit("alice", "r", "two_tries") // max_attempts 2
	e.mustClaim("w1")

	r := e.report("w1", id, 1, "retryable", map[string]any{"message": "downstream timeout"})
	want(t, "status", r.status, 200)
	want(t, "state", r.json()["state"], "queued")
	if r.json()["run_at"] == nil {
		t.Error("run_at should be set when the job is requeued")
	}
	// The wait after attempt 1 is 2 s plus up to 10% jitter (ADR 004).
	var secs float64
	e.pool.QueryRow(context.Background(), `SELECT extract(epoch FROM run_at - now()) FROM jobs WHERE id = $1`, id).Scan(&secs)
	if secs < 1.5 || secs > 2.3 {
		t.Errorf("run_at is %.2f s away, want about 2 to 2.2", secs)
	}
	j := e.job(id)
	want(t, "state", j["state"], "queued")
	want(t, "lease cleared", j["lease_owner"], nil)
	errs := j["errors"].([]any)
	want(t, "error entries", len(errs), 1)
	first := errs[0].(map[string]any)
	want(t, "kind", first["kind"], "retryable")
	want(t, "message", first["message"], "downstream timeout")
	want(t, "attempt", first["attempt"], float64(1))

	// Second attempt fails too: no attempts left, so the job is dead (T11).
	e.makeDue(id)
	w := e.mustClaim("w2")
	want(t, "attempt", w["attempt"], float64(2))
	r = e.report("w2", id, 2, "retryable", map[string]any{"message": "still down"})
	want(t, "state", r.json()["state"], "dead")
	want(t, "no run_at", r.json()["run_at"], nil)
	j = e.job(id)
	want(t, "state", j["state"], "dead")
	if j["finished_at"] == nil {
		t.Error("finished_at not set on a dead job")
	}
	want(t, "error entries", len(j["errors"].([]any)), 2)
}

func TestReportPermanentAndUnclassified(t *testing.T) {
	e := newEnv(t)
	// A permanent failure ends the job at once, even with attempts left (T10).
	id := e.mustSubmit("alice", "perm", "plain")
	e.mustClaim("w1")
	r := e.report("w1", id, 1, "permanent", map[string]any{"message": "400 from the API"})
	want(t, "permanent", r.json()["state"], "dead")
	want(t, "kind", e.job(id)["errors"].([]any)[0].(map[string]any)["kind"], "permanent")

	// An unclassified failure is retried like a retryable one, and recorded as its own kind.
	id2 := e.mustSubmit("alice", "unc", "plain")
	e.mustClaim("w1")
	r = e.report("w1", id2, 1, "unclassified", map[string]any{"message": "panic: nil map"})
	want(t, "unclassified", r.json()["state"], "queued")
	want(t, "kind", e.job(id2)["errors"].([]any)[0].(map[string]any)["kind"], "unclassified")

	// max_attempts 1: the first failure is the last (T11).
	id3 := e.mustSubmit("alice", "one", "one_try")
	e.mustClaim("w1")
	r = e.report("w1", id3, 1, "retryable", map[string]any{"message": "x"})
	want(t, "no attempts left", r.json()["state"], "dead")
}

func TestReportFencing(t *testing.T) {
	e := newEnv(t)
	id := e.mustSubmit("alice", "fence", "plain")
	e.mustClaim("w1")

	// The wrong worker, or the wrong attempt: 409, nothing changes.
	for name, c := range map[string]struct {
		who     string
		attempt int
	}{"wrong worker": {"w2", 1}, "wrong attempt": {"w1", 5}} {
		r := e.report(c.who, id, c.attempt, "success", nil)
		if r.status != 409 || r.errCode() != "lease_lost" {
			t.Errorf("%s: got %d %s", name, r.status, r.body)
		}
	}
	want(t, "still running", e.job(id)["state"], "running")

	// A late report from a worker that lost the job: the reaper requeues it with a lease_expired entry.
	e.expireLease(id)
	r := e.report("w1", id, 1, "success", nil)
	want(t, "late success", r.status, 409)
	want(t, "late success code", r.errCode(), "lease_lost")
	r = e.report("w1", id, 1, "retryable", map[string]any{"message": "late"})
	want(t, "late retryable (kind differs from lease_expired)", r.status, 409)
	want(t, "job is queued again", e.job(id)["state"], "queued")
	want(t, "no second error entry", len(e.job(id)["errors"].([]any)), 1)

	// A new attempt by another worker works, and the old worker still cannot report.
	e.makeDue(id)
	w := e.mustClaim("w2")
	want(t, "attempt 2", w["attempt"], float64(2))
	want(t, "old attempt is refused", e.report("w1", id, 1, "success", nil).status, 409)
	want(t, "new attempt accepted", e.report("w2", id, 2, "success", nil).status, 200)

	// The job has now succeeded at attempt 2. The old worker's success for attempt 1 must
	// not be answered as a repeat of the new one: it is refused.
	late := e.report("w1", id, 1, "success", nil)
	want(t, "stale success after a newer attempt succeeded", late.status, 409)
	want(t, "stale success code", late.errCode(), "lease_lost")
	// The new attempt's own success, repeated, is a replay.
	again := e.report("w2", id, 2, "success", nil)
	want(t, "repeat of the real success", again.json()["replayed"], true)
}

// expireLease makes a running job's lease run out and lets the real reaper take it back
// (T12). A running -> running update is the same shape as a heartbeat, so the triggers allow it.
func (e *env) expireLease(id string) {
	e.t.Helper()
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		e.t.Fatal(err)
	}
	if n, err := e.store.ReapExpiredLeases(ctx, 100); err != nil || n != 1 {
		e.t.Fatalf("reaper took back %d jobs, err %v; want 1", n, err)
	}
}

// A report that is repeated because the answer was lost gets 200 with replayed: true.
func TestReportReplay(t *testing.T) {
	e := newEnv(t)
	id := e.mustSubmit("alice", "rp", "plain")
	e.mustClaim("w1")
	first := e.report("w1", id, 1, "success", map[string]any{"result": 1})
	want(t, "first", first.json()["replayed"], false)
	again := e.report("w1", id, 1, "success", map[string]any{"result": 1})
	want(t, "again status", again.status, 200)
	want(t, "again replayed", again.json()["replayed"], true)
	want(t, "again state", again.json()["state"], "succeeded")

	id2 := e.mustSubmit("alice", "rp2", "plain")
	e.mustClaim("w1")
	f1 := e.report("w1", id2, 1, "retryable", map[string]any{"message": "x"})
	want(t, "failure first", f1.json()["replayed"], false)
	f2 := e.report("w1", id2, 1, "retryable", map[string]any{"message": "x"})
	want(t, "failure again status", f2.status, 200)
	want(t, "failure again replayed", f2.json()["replayed"], true)
	want(t, "failure again state", f2.json()["state"], "queued")
	if f2.json()["run_at"] == nil {
		t.Error("a replayed answer for a queued job should carry run_at")
	}
	want(t, "only one error entry", len(e.job(id2)["errors"].([]any)), 1)

	// A different outcome for the same attempt is not a replay.
	f3 := e.report("w1", id2, 1, "permanent", map[string]any{"message": "y"})
	want(t, "different outcome", f3.status, 409)
}

// Several copies of the same report arrive at once: one applies, the rest are replays.
func TestConcurrentReports(t *testing.T) {
	e := newEnv(t)
	id := e.mustSubmit("alice", "cr", "plain")
	e.mustClaim("w1")
	var wg sync.WaitGroup
	replays := make([]bool, 8)
	for i := range replays {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := e.report("w1", id, 1, "retryable", map[string]any{"message": "boom"})
			if r.status != 200 {
				t.Errorf("report %d: %d %s", i, r.status, r.body)
				return
			}
			replays[i] = r.json()["replayed"].(bool)
		}(i)
	}
	wg.Wait()
	applied := 0
	for _, rp := range replays {
		if !rp {
			applied++
		}
	}
	want(t, "reports applied", applied, 1)
	want(t, "error entries", len(e.job(id)["errors"].([]any)), 1)
}

func TestReportValidation(t *testing.T) {
	e := newEnv(t)
	id := e.mustSubmit("alice", "v", "plain")
	e.mustClaim("w1")
	path := "/jobs/" + id + "/report"
	for name, body := range map[string]string{
		"not json":           `x`,
		"no attempt":         `{"outcome":"success"}`,
		"attempt 0":          `{"attempt":0,"outcome":"success"}`,
		"unknown outcome":    `{"attempt":1,"outcome":"done"}`,
		"no outcome":         `{"attempt":1}`,
		"failure no message": `{"attempt":1,"outcome":"retryable"}`,
		"empty message":      `{"attempt":1,"outcome":"permanent","message":""}`,
	} {
		r := e.call("POST", path, "w1", nil, body)
		if r.status != 400 || r.errCode() != "invalid_request" {
			t.Errorf("%s: got %d %s", name, r.status, r.body)
		}
	}
	want(t, "no such job", e.report("w1", "6f1c0e5a-6a53-4d6e-9d57-0a2b7f2f6b11", 1, "success", nil).status, 404)
	want(t, "still running after refused reports", e.job(id)["state"], "running")
}

func TestReportResultSize(t *testing.T) {
	e := newEnv(t)
	id := e.mustSubmit("alice", "big", "plain")
	e.mustClaim("w1")
	over := strings.Repeat("a", 64*1024-1) // plus two quotes = 64 KB + 1
	r := e.report("w1", id, 1, "success", map[string]any{"result": over})
	want(t, "status", r.status, 413)
	want(t, "code", r.errCode(), "result_too_large")
	want(t, "nothing changed", e.job(id)["state"], "running")

	// The worker then reports a permanent failure (decision 2).
	r = e.report("w1", id, 1, "permanent", map[string]any{"message": "result too large"})
	want(t, "permanent after 413", r.json()["state"], "dead")

	// A result of exactly 64 KB is accepted.
	id2 := e.mustSubmit("alice", "big2", "plain")
	e.mustClaim("w1")
	atLimit := strings.Repeat("a", 64*1024-2)
	want(t, "result at the limit", e.report("w1", id2, 1, "success", map[string]any{"result": atLimit}).status, 200)
}

func TestReportMessageHandling(t *testing.T) {
	e := newEnv(t)
	// A message over 1,000 characters is cut, not refused. Count characters, not bytes.
	id := e.mustSubmit("alice", "m", "plain")
	e.mustClaim("w1")
	long := strings.Repeat("é", 1500)
	r := e.report("w1", id, 1, "retryable", map[string]any{"message": long})
	want(t, "status", r.status, 200)
	msg := e.job(id)["errors"].([]any)[0].(map[string]any)["message"].(string)
	want(t, "message length in characters", len([]rune(msg)), 1000)

	// A NUL character in a message is replaced, not an error.
	id2 := e.mustSubmit("alice", "m2", "plain")
	e.mustClaim("w1")
	r = e.report("w1", id2, 1, "unclassified", map[string]any{"message": "bad\u0000byte"})
	want(t, "NUL in message", r.status, 200)
	want(t, "NUL replaced", e.job(id2)["errors"].([]any)[0].(map[string]any)["message"], "bad\uFFFDbyte")
}

// ---------------------------------------------------------------------------
// Health
// ---------------------------------------------------------------------------

func TestHealthz(t *testing.T) {
	e := newEnv(t)
	r := e.call("GET", "/healthz", "", nil, nil)
	want(t, "status", r.status, 200)
	want(t, "body", r.json()["status"], "ok")

	// An engine that cannot reach its database answers 503. The pool connects lazily,
	// so this one is created without error and fails on the first query.
	dead, err := pgxpool.New(context.Background(), "host=127.0.0.1 port=1 user=nobody dbname=none connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer dead.Close()
	a := &api.API{Store: store.New(dead), Config: &config.Config{}, Log: slog.New(slog.NewTextHandler(testLog{t}, nil))}
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	want(t, "no database", res.StatusCode, 503)
	// Any call that needs the database says so too, and is not a 500.
	req, _ := http.NewRequest("POST", srv.URL+"/claim", nil)
	req.Header.Set("Authorization", "Bearer anything")
	res2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	want(t, "claim with no database", res2.StatusCode, 503)
}
