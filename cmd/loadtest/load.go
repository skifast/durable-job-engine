package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Load submits jobs through the API the way a client should: a fresh idempotency key per
// job, and the same key again when the answer is lost or the engine is unavailable.
type Load struct {
	rig     *Rig
	runID   string
	typ     string
	payload []byte
	http    *http.Client

	// RetryFor is how long a client keeps retrying one submit before it gives up on that job.
	RetryFor time.Duration

	n         atomic.Int64
	acked     atomic.Int64
	failed    atomic.Int64 // gave up
	retries   atomic.Int64 // extra requests made because an answer was lost or an error came back
	mu        sync.Mutex
	submitLat []time.Duration // the request time of each first try that returned 201
	ackedKeys []string
	ackedIDs  []string // job ids from the 201/200 answers, kept in memory so a database crash cannot lose them
	failedKey []string
}

func newLoad(rig *Rig, runID, typ string, payload any) *Load {
	raw, _ := json.Marshal(payload)
	return &Load{
		rig: rig, runID: runID, typ: typ, payload: raw, RetryFor: 60 * time.Second,
		http: &http.Client{
			Timeout:   10 * time.Second,
			Transport: &http.Transport{MaxIdleConnsPerHost: 512, MaxConnsPerHost: 0},
		},
	}
}

func (l *Load) KeyPrefix() string { return l.runID + "-" }

// submitOne submits one new job and retries it with the same key until it is acknowledged.
func (l *Load) submitOne(ctx context.Context) {
	key := fmt.Sprintf("%s%d", l.KeyPrefix(), l.n.Add(1))
	body := []byte(fmt.Sprintf(`{"type":%q,"payload":%s}`, l.typ, l.payload))
	start := time.Now()
	wait := 50 * time.Millisecond
	for try := 0; ; try++ {
		t0 := time.Now()
		status, id, err := l.post(ctx, key, body)
		if err == nil && (status == 201 || status == 200) {
			l.acked.Add(1)
			l.mu.Lock()
			l.ackedKeys = append(l.ackedKeys, key)
			l.ackedIDs = append(l.ackedIDs, id)
			if try == 0 {
				l.submitLat = append(l.submitLat, time.Since(t0))
			}
			l.mu.Unlock()
			return
		}
		// 4xx other than 409 would be a bug in the harness; do not retry it forever.
		permanent := err == nil && status >= 400 && status < 500 && status != 409
		if permanent || ctx.Err() != nil || time.Since(start) > l.RetryFor {
			l.failed.Add(1)
			l.mu.Lock()
			l.failedKey = append(l.failedKey, key)
			l.mu.Unlock()
			return
		}
		l.retries.Add(1)
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
		wait = min(wait*2, 2*time.Second)
	}
}

func (l *Load) post(ctx context.Context, key string, body []byte) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", l.rig.EngineURL()+"/jobs", bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token(clientKey))
	req.Header.Set("Idempotency-Key", key)
	res, err := l.http.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return 0, "", err // the answer was cut off: treat it as lost and retry with the same key
	}
	var job struct {
		ID string `json:"id"`
	}
	json.Unmarshal(b, &job)
	return res.StatusCode, job.ID, nil
}

// RunOpen submits at `rate` jobs per second for d, whatever the system's speed: a slow
// system builds a backlog instead of slowing the clients down. It returns when every
// submit has finished (acknowledged, or given up).
func (l *Load) RunOpen(ctx context.Context, rate float64, d time.Duration) {
	interval := time.Duration(float64(time.Second) / rate)
	start := time.Now()
	var wg sync.WaitGroup
	sem := make(chan struct{}, 2048)
	for i := 0; ; i++ {
		at := start.Add(time.Duration(i) * interval)
		if at.Sub(start) >= d || ctx.Err() != nil {
			break
		}
		if wait := time.Until(at); wait > 0 {
			time.Sleep(wait)
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			l.submitOne(ctx)
		}()
	}
	wg.Wait()
}

// RunClosed keeps `conc` submits in flight for d, as fast as the system answers.
func (l *Load) RunClosed(ctx context.Context, conc int, d time.Duration) {
	end := time.Now().Add(d)
	var wg sync.WaitGroup
	for i := 0; i < conc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(end) && ctx.Err() == nil {
				l.submitOne(ctx)
			}
		}()
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// Statistics
// ---------------------------------------------------------------------------

// Dist is a set of durations summarised, in milliseconds.
type Dist struct {
	N    int     `json:"n"`
	Mean float64 `json:"mean_ms"`
	P50  float64 `json:"p50_ms"`
	P95  float64 `json:"p95_ms"`
	P99  float64 `json:"p99_ms"`
	Max  float64 `json:"max_ms"`
}

func summarize(ds []time.Duration) Dist {
	if len(ds) == 0 {
		return Dist{}
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	var sum time.Duration
	for _, d := range s {
		sum += d
	}
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
	return Dist{
		N: len(s), Mean: ms(sum / time.Duration(len(s))),
		P50: ms(pct(s, 50)), P95: ms(pct(s, 95)), P99: ms(pct(s, 99)), Max: ms(s[len(s)-1]),
	}
}

// pct is the nearest-rank percentile of a sorted slice.
func pct(sorted []time.Duration, p float64) time.Duration {
	i := int(float64(len(sorted))*p/100+0.999999) - 1
	return sorted[max(0, min(i, len(sorted)-1))]
}

// ---------------------------------------------------------------------------
// Reading the results back from the database
// ---------------------------------------------------------------------------

// JobRow is what the harness reads about each job of a run.
type JobRow struct {
	Key        string
	State      string
	Attempt    int
	Created    time.Time
	Finished   *time.Time
	StartedAt  *time.Time // when the final attempt's handler began (from the handler's result)
	ErrorKinds []string
}

func (r *Rig) jobRows(ctx context.Context, prefix string) ([]JobRow, error) {
	rows, err := r.Pool.Query(ctx, `
		SELECT j.idempotency_key, j.state, j.attempt, j.created_at, j.finished_at,
		       (j.result->>'started_at')::timestamptz,
		       coalesce((SELECT array_agg(kind ORDER BY attempt) FROM job_errors e WHERE e.job_id = j.id), '{}')
		FROM jobs j
		WHERE j.client_id = $1 AND left(j.idempotency_key, length($2)) = $2`, clientKey, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobRow
	for rows.Next() {
		var j JobRow
		if err := rows.Scan(&j.Key, &j.State, &j.Attempt, &j.Created, &j.Finished, &j.StartedAt, &j.ErrorKinds); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// waitDrain waits until none of the run's jobs is queued, running or waiting, or until limit.
// It returns how long it waited and how many jobs were still unfinished.
func (r *Rig) waitDrain(ctx context.Context, prefix string, limit time.Duration) (time.Duration, int) {
	start := time.Now()
	for {
		var open int
		err := r.Pool.QueryRow(ctx, `
			SELECT count(*) FROM jobs
			WHERE client_id = $1 AND left(idempotency_key, length($2)) = $2
			  AND state IN ('queued', 'running', 'waiting_approval')`, clientKey, prefix).Scan(&open)
		if err == nil && open == 0 {
			return time.Since(start), 0
		}
		if time.Since(start) > limit || ctx.Err() != nil {
			return time.Since(start), open
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// Sampling the database while a run is going
// ---------------------------------------------------------------------------

// Sample is one look at the database.
type Sample struct {
	AtS          float64 `json:"t_s"`
	Queued       int     `json:"queued"`
	Running      int     `json:"running"`
	Succeeded    int     `json:"succeeded"`
	Dead         int     `json:"dead"`
	LiveTuples   int64   `json:"n_live_tup"`
	DeadTuples   int64   `json:"n_dead_tup"`
	Autovacuums  int64   `json:"autovacuum_count"`
	Autoanalyzes int64   `json:"autoanalyze_count"`
	TableBytes   int64   `json:"table_bytes"`
	TotalBytes   int64   `json:"table_with_indexes_bytes"`
	WALBytes     int64   `json:"wal_dir_bytes"`
	Connections  int     `json:"engine_db_connections"` // connections from the engines (not the harness)
}

func (r *Rig) sample(ctx context.Context, at time.Duration) (Sample, error) {
	s := Sample{AtS: at.Seconds()}
	rows, err := r.Pool.Query(ctx, `SELECT state, count(*) FROM jobs GROUP BY state`)
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var st string
		var n int
		rows.Scan(&st, &n)
		switch st {
		case "queued":
			s.Queued = n
		case "running":
			s.Running = n
		case "succeeded":
			s.Succeeded = n
		case "dead":
			s.Dead = n
		}
	}
	rows.Close()
	err = r.Pool.QueryRow(ctx, `
		SELECT n_live_tup, n_dead_tup, autovacuum_count, autoanalyze_count,
		       pg_relation_size('jobs'), pg_total_relation_size('jobs')
		FROM pg_stat_user_tables WHERE relname = 'jobs'`).
		Scan(&s.LiveTuples, &s.DeadTuples, &s.Autovacuums, &s.Autoanalyzes, &s.TableBytes, &s.TotalBytes)
	if err != nil {
		return s, err
	}
	_ = r.Pool.QueryRow(ctx, `SELECT coalesce(sum(size), 0) FROM pg_ls_waldir()`).Scan(&s.WALBytes)
	// The engines connect with no application_name; the harness names its own connections.
	_ = r.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
		WHERE datname = $1 AND backend_type = 'client backend' AND application_name = ''`, dbName).Scan(&s.Connections)
	return s, nil
}

// Sampler takes a sample every `every` until ctx ends.
type Sampler struct {
	rig     *Rig
	every   time.Duration
	started time.Time
	mu      sync.Mutex
	Samples []Sample
}

func (r *Rig) startSampler(ctx context.Context, every time.Duration) *Sampler {
	s := &Sampler{rig: r, every: every, started: time.Now()}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			if smp, err := r.sample(ctx, time.Since(s.started)); err == nil {
				s.mu.Lock()
				s.Samples = append(s.Samples, smp)
				s.mu.Unlock()
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return s
}

func (s *Sampler) Snapshot() []Sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Sample(nil), s.Samples...)
}
