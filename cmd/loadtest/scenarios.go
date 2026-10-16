package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/skifast/durable-job-engine/internal/store"
)

// Verdict is one pass/fail check. Pass is decided by the harness against the criterion in
// Detail; the criteria that the spec does not give a number for are the harness's own and
// are written up in docs/load-test-results.md.
type Verdict struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
}

// Report is what a scenario returns.
type Report struct {
	Scenario string         `json:"scenario"`
	Config   map[string]any `json:"config"`
	Verdicts []Verdict      `json:"verdicts"`
	Data     map[string]any `json:"data"`
}

func (r *Report) check(name string, pass bool, format string, args ...any) {
	r.Verdicts = append(r.Verdicts, Verdict{name, pass, fmt.Sprintf(format, args...)})
}

// RunSummary is the analysis of one load run.
type RunSummary struct {
	Submitted       int64          `json:"submitted"`
	Acked           int64          `json:"acked"`
	GaveUp          int64          `json:"gave_up"`
	ClientRetries   int64          `json:"client_retries"`
	SubmitLatency   Dist           `json:"submit_latency"`
	StartLatency    Dist           `json:"start_latency_all"`
	StartLatency1st Dist           `json:"start_latency_first_attempt"`
	Completion      Dist           `json:"completion_latency"`
	States          map[string]int `json:"states"`
	JobsRetried     int            `json:"jobs_with_more_than_one_attempt"`
	LeaseExpiries   int            `json:"lease_expired_entries"`
	SubmitRate      float64        `json:"submit_rate_per_s"`
	CompletionRate  float64        `json:"completion_rate_per_s_steady"`
	CompletionMin10 float64        `json:"completion_rate_per_s_worst_10s_window"`
	DrainSeconds    float64        `json:"drain_s"`
	Unfinished      int            `json:"unfinished_after_drain"`
	MissingAcked    int            `json:"acked_jobs_missing_from_table"`
	RowsForRun      int            `json:"rows_in_table_for_run"`
	Violations      map[string]int `json:"invariant_violations"`
	MaxQueued       int            `json:"max_queued_seen"`
}

// analyze reads the run back from the database and computes the numbers.
// steadyFrom is how long after runStart the steady window begins (warm-up is skipped).
func analyze(ctx context.Context, rig *Rig, l *Load, runStart, submitEnd time.Time, steadyFrom time.Duration,
	drain time.Duration, unfinished int, samples []Sample) (*RunSummary, error) {

	rows, err := rig.jobRows(ctx, l.KeyPrefix())
	if err != nil {
		return nil, err
	}
	s := &RunSummary{
		Submitted: l.n.Load(), Acked: l.acked.Load(), GaveUp: l.failed.Load(), ClientRetries: l.retries.Load(),
		SubmitLatency: summarize(l.submitLat), States: map[string]int{},
		DrainSeconds: drain.Seconds(), Unfinished: unfinished, RowsForRun: len(rows),
	}
	var start, start1, comp []time.Duration
	present := map[string]bool{}
	steadyStart, steadyEnd := runStart.Add(steadyFrom), submitEnd
	perSecond := map[int]int{}
	inWindow := 0
	for _, j := range rows {
		present[j.Key] = true
		s.States[j.State]++
		if j.Attempt > 1 {
			s.JobsRetried++
		}
		for _, k := range j.ErrorKinds {
			if k == "lease_expired" {
				s.LeaseExpiries++
			}
		}
		if j.StartedAt != nil {
			d := j.StartedAt.Sub(j.Created)
			start = append(start, d)
			if j.Attempt == 1 {
				start1 = append(start1, d)
			}
		}
		if j.Finished != nil {
			comp = append(comp, j.Finished.Sub(j.Created))
			if j.Finished.After(steadyStart) && !j.Finished.After(steadyEnd) {
				inWindow++
				perSecond[int(j.Finished.Sub(steadyStart).Seconds())]++
			}
		}
	}
	s.StartLatency, s.StartLatency1st, s.Completion = summarize(start), summarize(start1), summarize(comp)
	if w := steadyEnd.Sub(steadyStart).Seconds(); w > 0 {
		s.CompletionRate = float64(inWindow) / w
		s.SubmitRate = float64(l.acked.Load()) / submitEnd.Sub(runStart).Seconds()
		// The worst 10-second stretch inside the steady window.
		s.CompletionMin10 = -1
		for from := 0; float64(from+10) <= w; from++ {
			sum := 0
			for i := from; i < from+10; i++ {
				sum += perSecond[i]
			}
			if r := float64(sum) / 10; s.CompletionMin10 < 0 || r < s.CompletionMin10 {
				s.CompletionMin10 = r
			}
		}
	}
	for _, k := range l.ackedKeys {
		if !present[k] {
			s.MissingAcked++
		}
	}
	for _, smp := range samples {
		s.MaxQueued = max(s.MaxQueued, smp.Queued)
	}

	// Record every acknowledged id, then run the invariant view that includes "every
	// acknowledged job exists and is finished".
	if len(l.ackedIDs) > 0 {
		for i := 0; i < len(l.ackedIDs); i += 5000 {
			chunk := l.ackedIDs[i:min(i+5000, len(l.ackedIDs))]
			if _, err := rig.Pool.Exec(ctx, `INSERT INTO test_acknowledged_jobs (job_id)
				SELECT unnest($1::uuid[]) ON CONFLICT DO NOTHING`, chunk); err != nil {
				return nil, err
			}
		}
	}
	s.Violations, err = rig.violations(ctx)
	return s, err
}

func (r *Rig) violations(ctx context.Context) (map[string]int, error) {
	rows, err := r.Pool.Query(ctx, `SELECT invariant, count(*) FROM invariant_violations_after_drain GROUP BY invariant`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var name string
		var n int
		rows.Scan(&name, &n)
		out[name] = n
	}
	return out, rows.Err()
}

func (s *RunSummary) totalViolations() int {
	n := 0
	for _, v := range s.Violations {
		n += v
	}
	return n
}

// standardChecks are the checks every load run must pass.
func standardChecks(rep *Report, s *RunSummary) {
	rep.check("every acknowledged job is in the table", s.MissingAcked == 0, "%d of %d missing", s.MissingAcked, s.Acked)
	rep.check("no client gave up on a submit", s.GaveUp == 0, "%d gave up after retrying", s.GaveUp)
	rep.check("every job finished after the drain", s.Unfinished == 0, "%d unfinished", s.Unfinished)
	rep.check("every job succeeded (none dead)", s.States["dead"] == 0 && s.States["succeeded"] == int(s.Acked),
		"succeeded %d of %d acked, dead %d", s.States["succeeded"], s.Acked, s.States["dead"])
	rep.check("one row per key (no duplicates)", int64(s.RowsForRun) == s.Acked, "%d rows for %d acked keys", s.RowsForRun, s.Acked)
	rep.check("invariant views return zero rows", s.totalViolations() == 0, "%v", s.Violations)
}

// ---------------------------------------------------------------------------
// Throughput (spec section 3): 50 jobs/s for 5 minutes with the 100 ms handler
// ---------------------------------------------------------------------------

type cfg struct {
	handler        string
	rate           float64
	duration       time.Duration
	conc           int
	sampleEvery    time.Duration
	samples        int
	rows           int
	killEvery      time.Duration
	reps           int
	drainLimit     time.Duration
	windowForSoak  time.Duration
	downtime       time.Duration
	chaosKind      string
	chaosStartWait time.Duration
	stepRates      []float64
	stepDuration   time.Duration
}

func payloadFor(handler string) any {
	if handler == "sleep" {
		return map[string]int{"ms": 100}
	}
	return nil
}

func throughput(ctx context.Context, rig *Rig, c cfg) (*Report, error) {
	rep := &Report{Scenario: "throughput", Config: map[string]any{
		"handler": c.handler, "rate_per_s": c.rate, "duration": c.duration.String(), "workers": rig.Workers}, Data: map[string]any{}}
	l := newLoad(rig, "tp", c.handler, payloadFor(c.handler))
	sctx, stopSampling := context.WithCancel(ctx)
	smp := rig.startSampler(sctx, c.sampleEvery)

	runStart := time.Now()
	l.RunOpen(ctx, c.rate, c.duration)
	submitEnd := time.Now()
	drain, unfinished := rig.waitDrain(ctx, l.KeyPrefix(), c.drainLimit)
	stopSampling()

	s, err := analyze(ctx, rig, l, runStart, submitEnd, 10*time.Second, drain, unfinished, smp.Snapshot())
	if err != nil {
		return nil, err
	}
	rep.Data["summary"], rep.Data["samples"] = s, smp.Snapshot()
	standardChecks(rep, s)
	rep.check("sustained completion rate within 2% of the target", s.CompletionRate >= 0.98*c.rate,
		"%.1f jobs/s completed over the steady window (target %.0f); worst 10 s window %.1f", s.CompletionRate, c.rate, s.CompletionMin10)
	rep.check("no backlog at the end of the run", s.DrainSeconds < 10, "took %.1f s to drain after submitting stopped; max queued seen %d", s.DrainSeconds, s.MaxQueued)
	rep.Data["note"] = fmt.Sprintf("start latency under load (not the idle target): p50 %.0f ms, p95 %.0f ms, p99 %.0f ms", s.StartLatency.P50, s.StartLatency.P95, s.StartLatency.P99)
	return rep, nil
}

// ---------------------------------------------------------------------------
// Maximum throughput with the empty handler (reported, not pass/fail)
// ---------------------------------------------------------------------------

// Step is one rate of the ramp.
type Step struct {
	Rate         float64 `json:"target_rate_per_s"`
	SubmitRate   float64 `json:"submit_rate_per_s"`
	CompleteRate float64 `json:"completed_per_s"`
	QueuedAtEnd  int     `json:"queued_at_end"`
	QueuedAtBeg  int     `json:"queued_at_start"`
	StartP95ms   float64 `json:"start_latency_p95_ms"`
	Sustained    bool    `json:"sustained"`
}

// maxThroughput raises the submit rate step by step and reports the highest rate the system
// kept up with, from the API call to the finished job. A step is "sustained" if the jobs
// finished during it were at least 95% of the jobs submitted and the queue grew by no more
// than two seconds' worth of submits. The ramp stops at the first step that is not.
// (A first version submitted as fast as it could with no limit. That measured the submitters
// and the workers fighting for one Postgres and left 180,000 jobs unfinished.)
func maxThroughput(ctx context.Context, rig *Rig, c cfg) (*Report, error) {
	rep := &Report{Scenario: "max-throughput", Config: map[string]any{
		"handler": c.handler, "step_rates_per_s": c.stepRates, "step_duration": c.stepDuration.String(), "workers": rig.Workers}, Data: map[string]any{}}
	queued := func() int {
		var n int
		rig.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE state = 'queued'`).Scan(&n)
		return n
	}
	var steps []Step
	var loads []*Load
	best := 0.0
	for _, rate := range c.stepRates {
		l := newLoad(rig, fmt.Sprintf("mx%d", int(rate)), c.handler, payloadFor(c.handler))
		loads = append(loads, l)
		q0 := queued()
		runStart := time.Now()
		l.RunOpen(ctx, rate, c.stepDuration)
		runEnd := time.Now()
		q1 := queued()

		rows, err := rig.jobRows(ctx, l.KeyPrefix())
		if err != nil {
			return nil, err
		}
		// Jobs finished during the step include ones submitted in an earlier step, so count by
		// finish time across all steps so far.
		all, err := rig.jobRows(ctx, "mx")
		if err != nil {
			return nil, err
		}
		done := 0
		for _, j := range all {
			if j.Finished != nil && j.Finished.After(runStart) && !j.Finished.After(runEnd) {
				done++
			}
		}
		var st []time.Duration
		for _, j := range rows {
			if j.StartedAt != nil {
				st = append(st, j.StartedAt.Sub(j.Created))
			}
		}
		secs := runEnd.Sub(runStart).Seconds()
		step := Step{Rate: rate, SubmitRate: float64(l.acked.Load()) / secs, CompleteRate: float64(done) / secs,
			QueuedAtBeg: q0, QueuedAtEnd: q1, StartP95ms: summarize(st).P95}
		step.Sustained = step.CompleteRate >= 0.95*step.SubmitRate && q1-q0 <= int(2*rate)
		steps = append(steps, step)
		fmt.Printf("  step %.0f/s: submitted %.0f/s, completed %.0f/s, queue %d -> %d, start p95 %.0f ms, sustained=%v\n",
			rate, step.SubmitRate, step.CompleteRate, q0, q1, step.StartP95ms, step.Sustained)
		if !step.Sustained {
			break
		}
		best = step.CompleteRate
	}

	// Let everything finish, then check that nothing was lost.
	drain, unfinished := rig.waitDrain(ctx, "mx", c.drainLimit)
	var acked, gaveUp int64
	var ids, keys []string
	for _, l := range loads {
		acked += l.acked.Load()
		gaveUp += l.failed.Load()
		ids = append(ids, l.ackedIDs...)
		keys = append(keys, l.ackedKeys...)
	}
	rows, err := rig.jobRows(ctx, "mx")
	if err != nil {
		return nil, err
	}
	present := map[string]bool{}
	succeeded := 0
	for _, j := range rows {
		present[j.Key] = true
		if j.State == "succeeded" {
			succeeded++
		}
	}
	missing := 0
	for _, k := range keys {
		if !present[k] {
			missing++
		}
	}
	for i := 0; i < len(ids); i += 5000 {
		if _, err := rig.Pool.Exec(ctx, `INSERT INTO test_acknowledged_jobs (job_id) SELECT unnest($1::uuid[]) ON CONFLICT DO NOTHING`, ids[i:min(i+5000, len(ids))]); err != nil {
			return nil, err
		}
	}
	viol, err := rig.violations(ctx)
	if err != nil {
		return nil, err
	}
	nv := 0
	for _, v := range viol {
		nv += v
	}
	rep.Data["steps"] = steps
	rep.check("every acknowledged job is in the table", missing == 0, "%d of %d missing", missing, acked)
	rep.check("no client gave up on a submit", gaveUp == 0, "%d gave up", gaveUp)
	rep.check("every job finished after the drain", unfinished == 0, "%d unfinished after %.0f s", unfinished, drain.Seconds())
	rep.check("every job succeeded", int64(succeeded) == acked, "%d of %d", succeeded, acked)
	rep.check("invariant views return zero rows", nv == 0, "%v", viol)
	if best > 0 {
		rep.Data["reported"] = fmt.Sprintf("highest sustained rate: %.0f jobs/s completed (the empty handler, 10 workers, one Postgres, everything on one machine). Not pass/fail.", best)
	} else {
		rep.Data["reported"] = "the first step was not sustained"
	}
	return rep, nil
}

// ---------------------------------------------------------------------------
// Start latency on an idle system: p95 <= 250 ms
// ---------------------------------------------------------------------------

func startLatency(ctx context.Context, rig *Rig, c cfg) (*Report, error) {
	rep := &Report{Scenario: "start-latency-idle", Config: map[string]any{"jobs": c.samples, "workers": rig.Workers, "spacing": "random 100-300 ms"}, Data: map[string]any{}}
	l := newLoad(rig, "lat", "noop", nil)
	runStart := time.Now()
	// One job at a time, at random spacing, so each lands at a random point of the workers'
	// 100 ms polling cycle and the system is idle between jobs.
	for i := 0; i < c.samples && ctx.Err() == nil; i++ {
		l.submitOne(ctx)
		time.Sleep(time.Duration(100+rand.Intn(200)) * time.Millisecond)
	}
	submitEnd := time.Now()
	drain, unfinished := rig.waitDrain(ctx, l.KeyPrefix(), 30*time.Second)
	s, err := analyze(ctx, rig, l, runStart, submitEnd, 0, drain, unfinished, nil)
	if err != nil {
		return nil, err
	}
	rep.Data["summary"] = s
	standardChecks(rep, s)
	rep.check("start latency p95 <= 250 ms", s.StartLatency.P95 <= 250, "p50 %.0f ms, p95 %.0f ms, p99 %.0f ms, max %.0f ms over %d jobs",
		s.StartLatency.P50, s.StartLatency.P95, s.StartLatency.P99, s.StartLatency.Max, s.StartLatency.N)
	return rep, nil
}

// ---------------------------------------------------------------------------
// Backlog: claim latency with 100,000 queued rows vs a near-empty queue
// ---------------------------------------------------------------------------

type claimer struct {
	rig  *Rig
	http *http.Client
	key  string
}

func (c *claimer) claim(ctx context.Context) (time.Duration, int, string, int, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", c.rig.EngineURL()+"/claim", nil)
	req.Header.Set("Authorization", "Bearer "+token(c.key))
	t0 := time.Now()
	res, err := c.http.Do(req)
	if err != nil {
		return 0, 0, "", 0, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	d := time.Since(t0)
	var it struct {
		ID      string `json:"id"`
		Attempt int    `json:"attempt"`
	}
	json.Unmarshal(b, &it)
	return d, res.StatusCode, it.ID, it.Attempt, nil
}

func (c *claimer) reportSuccess(ctx context.Context, id string, attempt int) error {
	body, _ := json.Marshal(map[string]any{"attempt": attempt, "outcome": "success"})
	req, _ := http.NewRequestWithContext(ctx, "POST", c.rig.EngineURL()+"/jobs/"+id+"/report", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token(c.key))
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	return nil
}

func backlog(ctx context.Context, rig *Rig, c cfg) (*Report, error) {
	rep := &Report{Scenario: "backlog", Config: map[string]any{"backlog_rows": c.rows, "claims_each": c.samples}, Data: map[string]any{}}
	if _, err := rig.Pool.Exec(ctx, `INSERT INTO api_keys (name, role, key_hash) VALUES ('claimer', 'worker', $1)`, store.HashKey(token("claimer"))); err != nil {
		return nil, err
	}
	cl := &claimer{rig: rig, http: &http.Client{Timeout: 20 * time.Second}, key: "claimer"}

	// Baseline A: the queue is empty, so each claim finds nothing (204).
	var empty []time.Duration
	for i := 0; i < c.samples; i++ {
		d, status, _, _, err := cl.claim(ctx)
		if err != nil || status != 204 {
			return nil, fmt.Errorf("empty-queue claim: status %d err %v", status, err)
		}
		empty = append(empty, d)
	}
	// Baseline B: exactly one due job, so each claim finds one. This is the like-for-like
	// comparison for a claim against a deep queue, which also finds a job.
	l := newLoad(rig, "base", "noop", nil)
	var one []time.Duration
	for i := 0; i < c.samples; i++ {
		l.submitOne(ctx)
		d, status, id, attempt, err := cl.claim(ctx)
		if err != nil || status != 200 {
			return nil, fmt.Errorf("baseline claim: status %d err %v", status, err)
		}
		one = append(one, d)
		if err := cl.reportSuccess(ctx, id, attempt); err != nil {
			return nil, err
		}
	}

	// Fill the queue. Rows go in by SQL because 100,000 API submits would take minutes
	// and the claim path does not care how a row got there. The rows are ordinary: valid
	// hash, state queued, due now.
	t0 := time.Now()
	for done := 0; done < c.rows; done += 20000 {
		n := min(20000, c.rows-done)
		if _, err := rig.Pool.Exec(ctx, `
			INSERT INTO jobs (type, client_id, idempotency_key, payload_hash, payload, state, max_attempts)
			SELECT 'noop', 'backlog-filler', 'f' || g, md5(g::text) || md5(g::text), '{}'::jsonb, 'queued', 8
			FROM generate_series($1::int, $2::int) g`, done+1, done+n); err != nil {
			return nil, err
		}
	}
	// ANALYZE is what autovacuum would do soon after a bulk load.
	if _, err := rig.Pool.Exec(ctx, `ANALYZE jobs`); err != nil {
		return nil, err
	}
	var queued int
	rig.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE state = 'queued'`).Scan(&queued)
	rep.Data["fill_seconds"] = time.Since(t0).Seconds()
	rep.Data["queued_rows_before_measuring"] = queued

	// Measure: one client, claims one after another.
	var deep []time.Duration
	for i := 0; i < c.samples; i++ {
		d, status, _, _, err := cl.claim(ctx)
		if err != nil || status != 200 {
			return nil, fmt.Errorf("deep claim: status %d err %v", status, err)
		}
		deep = append(deep, d)
	}
	// And ten clients at once, as ten workers would.
	var deepConc []time.Duration
	results := make(chan []time.Duration, 10)
	for w := 0; w < 10; w++ {
		go func() {
			cc := &claimer{rig: rig, http: &http.Client{Timeout: 20 * time.Second}, key: "claimer"}
			var ds []time.Duration
			for i := 0; i < c.samples/2; i++ {
				if d, status, _, _, err := cc.claim(ctx); err == nil && status == 200 {
					ds = append(ds, d)
				}
			}
			results <- ds
		}()
	}
	for w := 0; w < 10; w++ {
		deepConc = append(deepConc, <-results...)
	}

	e, o, d, dc := summarize(empty), summarize(one), summarize(deep), summarize(deepConc)
	rep.Data["claim_empty_queue_204"], rep.Data["claim_one_due_job"] = e, o
	rep.Data["claim_100k_queue_one_client"], rep.Data["claim_100k_queue_ten_clients"] = d, dc
	rep.check("100k rows were queued while measuring", queued >= c.rows, "%d queued", queued)
	rep.check("claim p95 with 100k queued is within 2x of the claim p95 with one due job",
		d.P95 <= 2*o.P95, "%.1f ms vs 2 x %.1f ms = %.1f ms (one client); ten clients at once: p95 %.1f ms", d.P95, o.P95, 2*o.P95, dc.P95)
	v, err := rig.violations(ctx)
	if err != nil {
		return nil, err
	}
	n := 0
	for _, c := range v {
		n += c
	}
	// Jobs claimed here are left running on purpose; only the plain invariants apply.
	var plain int
	rig.Pool.QueryRow(ctx, `SELECT count(*) FROM invariant_violations`).Scan(&plain)
	rep.check("invariant view returns zero rows", plain == 0, "%d violations", plain)
	_ = n
	return rep, nil
}

// ---------------------------------------------------------------------------
// Soak: 30 minutes at the target rate; does vacuum keep up?
// ---------------------------------------------------------------------------

type window struct {
	FromMin       float64 `json:"from_min"`
	Jobs          int     `json:"jobs"`
	StartP95ms    float64 `json:"start_latency_p95_ms"`
	CompletionP95 float64 `json:"completion_latency_p95_ms"`
}

func soak(ctx context.Context, rig *Rig, c cfg) (*Report, error) {
	rep, err := throughput(ctx, rig, c)
	if err != nil {
		return nil, err
	}
	rep.Scenario = "soak"
	rep.Config["sample_every"] = c.sampleEvery.String()
	samples, _ := rep.Data["samples"].([]Sample)

	// Latency per window (by when the job was created), to see whether it drifts.
	l := &Load{runID: "tp"}
	rows, err := rig.jobRows(ctx, l.KeyPrefix())
	if err != nil {
		return nil, err
	}
	var wins []window
	for from := time.Duration(0); from < c.duration; from += c.windowForSoak {
		var first time.Time
		for _, j := range rows {
			if first.IsZero() || j.Created.Before(first) {
				first = j.Created
			}
		}
		var st, cp []time.Duration
		for _, j := range rows {
			off := j.Created.Sub(first)
			if off >= from && off < from+c.windowForSoak && j.StartedAt != nil && j.Finished != nil {
				st = append(st, j.StartedAt.Sub(j.Created))
				cp = append(cp, j.Finished.Sub(j.Created))
			}
		}
		if len(st) > 0 {
			wins = append(wins, window{from.Minutes(), len(st), summarize(st).P95, summarize(cp).P95})
		}
	}
	rep.Data["windows"] = wins

	if len(samples) >= 4 && len(wins) >= 2 {
		var maxDead int64
		for _, s := range samples {
			maxDead = max(maxDead, s.DeadTuples)
		}
		last := samples[len(samples)-1]
		// Bytes per live row, early (after 10% of the run) and at the end: bloat shows as growth.
		mid := samples[len(samples)/10+1]
		perRow := func(s Sample) float64 { return float64(s.TableBytes) / float64(max(1, s.LiveTuples)) }
		rep.Data["table_bytes_per_live_row_early"], rep.Data["table_bytes_per_live_row_end"] = perRow(mid), perRow(last)
		rep.Data["max_n_dead_tup"] = maxDead
		rep.check("autovacuum ran during the soak", last.Autovacuums > 0, "%d autovacuum runs on the jobs table", last.Autovacuums)
		rep.check("no table bloat: bytes per live row at the end <= 1.5x the early value",
			perRow(last) <= 1.5*perRow(mid), "%.0f bytes/row early, %.0f at the end; max n_dead_tup seen %d", perRow(mid), perRow(last), maxDead)
		rep.check("latency does not drift: last window's start p95 <= 2x the first window's (or under 250 ms)",
			wins[len(wins)-1].StartP95ms <= max(2*wins[0].StartP95ms, 250), "first window p95 %.0f ms, last %.0f ms", wins[0].StartP95ms, wins[len(wins)-1].StartP95ms)
	}
	return rep, nil
}

// ---------------------------------------------------------------------------
// Chaos: kill things during load
// ---------------------------------------------------------------------------

type event struct {
	AtS    float64 `json:"t_s"`
	What   string  `json:"what"`
	Detail string  `json:"detail,omitempty"`
}

func chaos(ctx context.Context, rig *Rig, c cfg) (*Report, error) {
	rep := &Report{Scenario: "chaos-" + c.chaosKind, Config: map[string]any{
		"kind": c.chaosKind, "rate_per_s": c.rate, "duration": c.duration.String(), "kill_every": c.killEvery.String(),
		"downtime": c.downtime.String(), "workers": rig.Workers}, Data: map[string]any{}}
	l := newLoad(rig, "ch", "sleep", map[string]int{"ms": 100})
	sctx, stop := context.WithCancel(ctx)
	smp := rig.startSampler(sctx, 5*time.Second)

	var events []event
	runStart := time.Now()
	note := func(what, detail string) {
		events = append(events, event{time.Since(runStart).Seconds(), what, detail})
	}

	chaosDone := make(chan struct{})
	go func() {
		defer close(chaosDone)
		t := time.NewTimer(c.chaosStartWait)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if time.Since(runStart) > c.duration-c.killEvery/2 {
				return // leave the last stretch calm, so the system can catch up under load
			}
			switch c.chaosKind {
			case "worker":
				i := rand.Intn(rig.Workers)
				note("kill -9 worker", fmt.Sprint(i+1))
				rig.KillWorker(i)
				time.Sleep(c.downtime)
				if err := rig.RestartWorker(i); err != nil {
					note("restart failed", err.Error())
				}
			case "engine":
				note("kill -9 engine (API server and reaper)", "")
				rig.KillEngine()
				time.Sleep(c.downtime)
				if err := rig.RestartEngine(); err != nil {
					note("restart failed", err.Error())
				}
				note("engine healthy again", "")
			case "postgres":
				note("kill -9 postgres", "")
				rig.KillPostgres()
				time.Sleep(c.downtime)
				t0 := time.Now()
				if err := rig.RestartPostgres(); err != nil {
					note("restart failed", err.Error())
				}
				note("postgres accepting connections again", fmt.Sprintf("recovery took %.1f s", time.Since(t0).Seconds()))
			}
			t.Reset(c.killEvery)
		}
	}()

	l.RunOpen(ctx, c.rate, c.duration)
	submitEnd := time.Now()
	<-chaosDone
	// Everything that was killed is running again before the drain starts.
	drain, unfinished := rig.waitDrain(ctx, l.KeyPrefix(), c.drainLimit)
	stop()

	s, err := analyze(ctx, rig, l, runStart, submitEnd, 0, drain, unfinished, smp.Snapshot())
	if err != nil {
		return nil, err
	}
	rep.Data["summary"], rep.Data["events"], rep.Data["samples"] = s, events, smp.Snapshot()
	standardChecks(rep, s)
	kills := 0
	for _, e := range events {
		if strings.HasPrefix(e.What, "kill") {
			kills++
		}
	}
	rep.check("the chaos actually happened", kills >= 2, "%d kills", kills)
	rep.Data["note"] = fmt.Sprintf("%d kills; %d jobs needed more than one attempt; %d lease expiries; %d client retries; completion latency p99 %.0f ms, max %.0f ms; drained in %.0f s",
		kills, s.JobsRetried, s.LeaseExpiries, s.ClientRetries, s.Completion.P99, s.Completion.Max, s.DrainSeconds)
	return rep, nil
}

// ---------------------------------------------------------------------------
// Lease recovery: kill a worker mid-job, time until the job runs elsewhere (about 37 s)
// ---------------------------------------------------------------------------

func (r *Rig) jobByKey(ctx context.Context, key string) (id, state string, attempt int, owner *string, err error) {
	err = r.Pool.QueryRow(ctx, `SELECT id, state, attempt, lease_owner FROM jobs WHERE client_id = $1 AND idempotency_key = $2`,
		clientKey, key).Scan(&id, &state, &attempt, &owner)
	return
}

func leaseRecovery(ctx context.Context, rig *Rig, c cfg) (*Report, error) {
	rep := &Report{Scenario: "lease-recovery", Config: map[string]any{"repetitions": c.reps, "workers": rig.Workers}, Data: map[string]any{}}
	var times []time.Duration
	for rep1 := 0; rep1 < c.reps; rep1++ {
		l := newLoad(rig, fmt.Sprintf("lr%d", rep1), "sleep_long", map[string]int{"ms": 3000})
		l.submitOne(ctx)
		key := l.KeyPrefix() + "1"
		var id string
		var owner *string
		for {
			var state string
			var attempt int
			var err error
			id, state, attempt, owner, err = rig.jobByKey(ctx, key)
			if err == nil && state == "running" && attempt == 1 && owner != nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		var idx int
		fmt.Sscanf(*owner, "worker-%d", &idx)
		idx-- // workers are numbered from 1
		killedAt := time.Now()
		rig.KillWorker(idx)
		// The handler needs 3 s, so the job was certainly still running when the worker died.
		var ranElsewhereAt time.Time
		for time.Since(killedAt) < 120*time.Second {
			_, state, attempt, _, err := rig.jobByKey(ctx, key)
			if err == nil && attempt == 2 && (state == "running" || state == "succeeded") {
				ranElsewhereAt = time.Now()
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if ranElsewhereAt.IsZero() {
			rep.check(fmt.Sprintf("repetition %d: job ran again", rep1+1), false, "not within 120 s")
			continue
		}
		times = append(times, ranElsewhereAt.Sub(killedAt))
		if err := rig.RestartWorker(idx); err != nil {
			return nil, err
		}
		if d, left := rig.waitDrain(ctx, l.KeyPrefix(), 30*time.Second); left != 0 {
			rep.check(fmt.Sprintf("repetition %d: job finished", rep1+1), false, "still unfinished after %v", d)
		}
		_ = id
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	secs := make([]float64, len(times))
	for i, t := range times {
		secs[i] = t.Seconds()
	}
	rep.Data["seconds_from_kill_to_running_elsewhere"] = secs
	if len(times) > 0 {
		rep.check("worst case <= 40 s (30 s lease + 5 s reaper + 2 s first retry delay + polling; spec says about 37 s)",
			times[len(times)-1] <= 40*time.Second, "fastest %.1f s, median %.1f s, slowest %.1f s over %d kills",
			times[0].Seconds(), times[len(times)/2].Seconds(), times[len(times)-1].Seconds(), len(times))
	}
	var plain int
	rig.Pool.QueryRow(ctx, `SELECT count(*) FROM invariant_violations`).Scan(&plain)
	rep.check("invariant view returns zero rows", plain == 0, "%d violations", plain)
	return rep, nil
}

// ---------------------------------------------------------------------------
// Pause a worker past its lease: its late report must be refused
// ---------------------------------------------------------------------------

func pauseWorker(ctx context.Context, rig *Rig, c cfg) (*Report, error) {
	rep := &Report{Scenario: "pause-worker", Config: map[string]any{"workers": rig.Workers, "pause": "until the job runs on another worker"}, Data: map[string]any{}}
	l := newLoad(rig, "pw", "sleep_long", map[string]int{"ms": 20000})
	l.submitOne(ctx)
	key := l.KeyPrefix() + "1"
	var id string
	var owner *string
	for {
		var state string
		var attempt int
		var err error
		id, state, attempt, owner, err = rig.jobByKey(ctx, key)
		if err == nil && state == "running" && attempt == 1 && owner != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	var idx int
	fmt.Sscanf(*owner, "worker-%d", &idx)
	paused := rig.workers[idx-1]
	pausedAt := time.Now()
	paused.signal(syscall.SIGSTOP) // frozen: no heartbeats, like a long GC pause or a stopped VM
	rep.Data["paused_worker"] = *owner

	var takenOverAfter time.Duration
	for time.Since(pausedAt) < 120*time.Second {
		_, state, attempt, o, err := rig.jobByKey(ctx, key)
		if err == nil && attempt == 2 && state == "running" && o != nil && *o != *owner {
			takenOverAfter = time.Since(pausedAt)
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	rep.check("another worker took the job over", takenOverAfter > 0, "after %.1f s", takenOverAfter.Seconds())

	// The frozen worker "wakes up" and reports success for attempt 1, as a worker coming
	// back from a long pause would. Sent here with its own key, since the real process is frozen.
	body, _ := json.Marshal(map[string]any{"attempt": 1, "outcome": "success", "result": map[string]string{"late": "yes"}})
	req, _ := http.NewRequestWithContext(ctx, "POST", rig.EngineURL()+"/jobs/"+id+"/report", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token(*owner))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	rep.check("the late report is refused with 409 lease_lost", res.StatusCode == 409 && bytes.Contains(b, []byte("lease_lost")), "%d %s", res.StatusCode, bytes.TrimSpace(b))

	// Now let the process continue. Its handler timer ran out while it was frozen, so one of
	// two things happens, depending on which the worker sees first: the handler returns and
	// the worker sends its own late report (the engine refuses it), or its next heartbeat gets
	// 409 and it cancels the handler. Either is correct. What must not happen is an accepted
	// success report from the old attempt.
	paused.signal(syscall.SIGCONT)
	const cancelled, refused = "lease lost; handler cancelled", "report refused: the job is no longer ours"
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && !logContains(paused.logPath, cancelled) && !logContains(paused.logPath, refused) {
		time.Sleep(200 * time.Millisecond)
	}
	which := "neither"
	switch {
	case logContains(paused.logPath, refused):
		which = "its own late report was refused (409)"
	case logContains(paused.logPath, cancelled):
		which = "its heartbeat got 409 and it cancelled the handler"
	}
	accepted := logContains(paused.logPath, "msg=reported") // the worker logs "reported" only when the engine accepted
	rep.check("the resumed worker was stopped by the fence and no report of the old attempt was accepted",
		which != "neither" && !accepted, "%s; accepted report in its log: %v", which, accepted)

	drain, left := rig.waitDrain(ctx, l.KeyPrefix(), 60*time.Second)
	_ = drain
	rows, err := rig.jobRows(ctx, l.KeyPrefix())
	if err != nil {
		return nil, err
	}
	ok := len(rows) == 1 && rows[0].State == "succeeded" && rows[0].Attempt == 2 && len(rows[0].ErrorKinds) == 1 && rows[0].ErrorKinds[0] == "lease_expired" && left == 0
	rep.check("the job finished once, on attempt 2, with one lease_expired entry", ok, "%+v", rows)
	var res2 string
	rig.Pool.QueryRow(ctx, `SELECT coalesce(result::text, '') FROM jobs WHERE id = $1`, id).Scan(&res2)
	rep.check("the stored result is the new attempt's, not the late one", !strings.Contains(res2, "late"), "%s", res2)
	var viol int
	rig.Pool.QueryRow(ctx, `SELECT count(*) FROM invariant_violations`).Scan(&viol)
	rep.check("invariant view returns zero rows", viol == 0, "%d violations", viol)
	return rep, nil
}
