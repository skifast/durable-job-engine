// Command loadtest runs the load, soak and chaos tests of spec sections 3, 8 and 12.
//
// It starts its own Postgres cluster (stock settings, in a scratch directory), the engine
// and the workers, all on this machine, then drives load through the public API and reads
// the results back from the database. Run it from the repository root:
//
//	go run ./cmd/loadtest throughput -dir /tmp/lt -out results/throughput.json
//
// Scenarios: throughput, max-throughput, start-latency, backlog, soak, chaos-worker,
// chaos-engine, chaos-postgres, lease-recovery, pause-worker, and for several engines (-engines N):
// chaos-one-engine, rolling-restart, same-key, reaper-recovery. See docs/adr/016-running-several-engines.md.
// The exit status is 1 if any check fails.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: loadtest <scenario> [flags]; see the comment at the top of main.go")
		os.Exit(2)
	}
	name := os.Args[1]
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	var (
		dir     = fs.String("dir", "", "scratch directory for the cluster, binaries and logs (required; wiped at the start)")
		out     = fs.String("out", "", "write the JSON report here")
		pgBin   = fs.String("pgbin", "", "Postgres bin directory (default: from pg_config)")
		workers = fs.Int("workers", 10, "number of worker processes")
		engines = fs.Int("engines", 1, "number of engines (more than one puts a round-robin proxy in front of them)")
		useLB   = fs.Bool("lb", false, "put the proxy in front of a single engine too (so one-engine and many-engine runs are comparable)")
		dbConns = fs.Int("db-conns", 10, "each engine's DB_MAX_CONNS")
		c       cfg
	)
	fs.StringVar(&c.handler, "handler", "sleep", "job type for throughput runs: sleep (100 ms) or noop")
	fs.Float64Var(&c.rate, "rate", 50, "jobs per second")
	fs.DurationVar(&c.duration, "duration", 5*time.Minute, "how long to submit")
	steps := fs.String("steps", "200,400,800,1200,1600,2400,3200,4800", "max-throughput: submit rates (jobs/s) to try, in order")
	fs.DurationVar(&c.stepDuration, "step-duration", 30*time.Second, "max-throughput: how long to hold each rate")
	fs.DurationVar(&c.sampleEvery, "sample-every", 5*time.Second, "how often to sample the database")
	fs.IntVar(&c.samples, "samples", 300, "jobs or claims to measure")
	fs.IntVar(&c.rows, "rows", 100000, "backlog size")
	fs.DurationVar(&c.killEvery, "kill-every", 15*time.Second, "chaos: time between kills")
	fs.DurationVar(&c.downtime, "downtime", time.Second, "chaos: how long a killed thing stays down")
	fs.DurationVar(&c.chaosStartWait, "chaos-start", 20*time.Second, "chaos: time before the first kill")
	fs.IntVar(&c.reps, "reps", 5, "lease-recovery repetitions")
	fs.IntVar(&c.leases, "leases", 1000, "reaper-recovery: how many expired leases")
	fs.DurationVar(&c.drainLimit, "drain-limit", 3*time.Minute, "how long to wait for jobs to finish after submitting stops")
	fs.DurationVar(&c.windowForSoak, "window", 5*time.Minute, "soak: latency window")
	fs.Parse(os.Args[2:])
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "-dir is required")
		os.Exit(2)
	}
	if *pgBin == "" {
		b, err := exec.Command("pg_config", "--bindir").Output()
		if err != nil {
			fmt.Fprintln(os.Stderr, "cannot find Postgres: pass -pgbin")
			os.Exit(2)
		}
		*pgBin = strings.TrimSpace(string(b))
	}

	needWorkers := *workers
	switch name {
	case "backlog":
		needWorkers = 0 // the harness does the claiming itself
	case "lease-recovery", "pause-worker":
		needWorkers = 2
	case "reaper-recovery", "same-key":
		needWorkers = 0 // nothing may claim the jobs the test sets up
	case "chaos-worker", "chaos-engine", "chaos-postgres", "chaos-one-engine", "rolling-restart":
		c.chaosKind = strings.TrimPrefix(name, "chaos-")
		if name == "rolling-restart" {
			c.chaosKind = "rolling"
		}
		if (c.chaosKind == "one-engine" || c.chaosKind == "rolling") && *engines < 2 {
			fatal(fmt.Errorf("%s needs -engines 2 or more", name))
		}
		switch c.chaosKind {
		case "one-engine":
			if !flagSet(fs, "kill-every") {
				c.killEvery = 20 * time.Second
			}
			if !flagSet(fs, "downtime") {
				c.downtime = 2 * time.Second
			}
		case "rolling":
			if !flagSet(fs, "kill-every") {
				c.killEvery = 60 * time.Second
			}
		case "engine":
			if !flagSet(fs, "kill-every") {
				c.killEvery = 30 * time.Second
			}
			if !flagSet(fs, "downtime") {
				c.downtime = 2 * time.Second
			}
		case "postgres":
			if !flagSet(fs, "kill-every") {
				c.killEvery = 60 * time.Second
			}
			if !flagSet(fs, "downtime") {
				c.downtime = 5 * time.Second
			}
			if !flagSet(fs, "chaos-start") {
				c.chaosStartWait = 30 * time.Second
			}
		}
		if !flagSet(fs, "duration") {
			c.duration = 3 * time.Minute
		}
	case "max-throughput":
		if !flagSet(fs, "handler") {
			c.handler = "noop"
		}
	case "soak":
		if !flagSet(fs, "duration") {
			c.duration = 30 * time.Minute
		}
		if !flagSet(fs, "sample-every") {
			c.sampleEvery = 30 * time.Second
		}
	}

	for _, f := range strings.Split(*steps, ",") {
		var r float64
		if _, err := fmt.Sscan(strings.TrimSpace(f), &r); err != nil || r <= 0 {
			fatal(fmt.Errorf("bad -steps value %q", f))
		}
		c.stepRates = append(c.stepRates, r)
	}

	repo, err := os.Getwd()
	if err != nil {
		fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, "go.mod")); err != nil {
		fatal(fmt.Errorf("run this from the repository root"))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("== %s: starting Postgres, engine and %d workers in %s\n", name, needWorkers, *dir)
	rig, err := newRig(repo, *dir, *pgBin, needWorkers, rigOptions{Engines: *engines, UseLB: *useLB, DBConns: *dbConns})
	if err != nil {
		fatal(err)
	}
	defer rig.Close()

	// A hot laptop slows itself down, and numbers taken while it does so are not the system's. Wait
	// for the CPU to cool, and watch it during the run.
	var lowestSpeed atomic.Int32
	lowestSpeed.Store(int32(waitForCool(ctx, 20*time.Minute)))
	thermalCtx, stopThermal := context.WithCancel(ctx)
	go watchThermal(thermalCtx, &lowestSpeed)

	started := time.Now()
	var rep *Report
	switch name {
	case "throughput":
		rep, err = throughput(ctx, rig, c)
	case "max-throughput":
		rep, err = maxThroughput(ctx, rig, c)
	case "start-latency":
		rep, err = startLatency(ctx, rig, c)
	case "backlog":
		rep, err = backlog(ctx, rig, c)
	case "soak":
		rep, err = soak(ctx, rig, c)
	case "chaos-worker", "chaos-engine", "chaos-postgres", "chaos-one-engine", "rolling-restart":
		rep, err = chaos(ctx, rig, c)
	case "same-key":
		rep, err = sameKey(ctx, rig, c)
	case "reaper-recovery":
		rep, err = reaperRecovery(ctx, rig, c)
	case "lease-recovery":
		rep, err = leaseRecovery(ctx, rig, c)
	case "pause-worker":
		rep, err = pauseWorker(ctx, rig, c)
	default:
		err = fmt.Errorf("unknown scenario %q", name)
	}
	if err != nil {
		fatal(err)
	}
	rep.Data["environment"] = environment(*pgBin, rig)
	rep.Data["wall_clock_seconds"] = time.Since(started).Seconds()

	// A laptop that sleeps in the middle of a run ruins its numbers: the database stamps jobs with
	// the wall clock, which keeps counting, while Go's own timers do not. Comparing the two clocks
	// shows whether the machine slept. (A run once spent about 40 minutes asleep and reported
	// nonsense latencies; this check exists so that cannot pass unnoticed again.)
	monotonic := time.Since(started)
	wall := time.Now().Round(0).Sub(started.Round(0)) // Round(0) drops the monotonic reading
	stopThermal()
	rep.Data["cpu_speed_limit_lowest_percent"] = lowestSpeed.Load()
	if handled, _ := rep.Data["thermal_handled"].(bool); !handled {
		rep.check("the CPU was not thermally throttled during the run", lowestSpeed.Load() >= 100,
			"lowest CPU speed limit seen: %d%% (100 is unthrottled; a hot laptop lowers it and the numbers are then not the system's)", lowestSpeed.Load())
	}
	slept := wall - monotonic
	rep.check("the machine did not sleep during the run", slept < 2*time.Second,
		"the wall clock moved %.1f s and the run's own timer %.1f s (a gap means the machine slept: keep it awake, for example with caffeinate, and rerun)", wall.Seconds(), monotonic.Seconds())

	failed := false
	fmt.Printf("\n== %s results\n", rep.Scenario)
	for _, v := range rep.Verdicts {
		mark := "PASS"
		if !v.Pass {
			mark, failed = "FAIL", true
		}
		fmt.Printf("  %s  %s\n        %s\n", mark, v.Name, v.Detail)
	}
	for _, k := range []string{"note", "reported"} {
		if s, ok := rep.Data[k].(string); ok {
			fmt.Printf("  %s: %s\n", k, s)
		}
	}
	if *out != "" {
		if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
			fatal(err)
		}
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(*out, b, 0o644); err != nil {
			fatal(err)
		}
		fmt.Println("  report written to", *out)
	}
	if failed {
		rig.Close()
		os.Exit(1)
	}
}

func flagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

// environment records the hardware and software, so the numbers can be read in context.
func environment(pgBin string, rig *Rig) map[string]string {
	run := func(name string, args ...string) string {
		b, err := exec.Command(name, args...).Output()
		if err != nil {
			return "unknown"
		}
		return strings.TrimSpace(string(b))
	}
	env := map[string]string{
		"os":       runtime.GOOS + "/" + runtime.GOARCH,
		"go":       runtime.Version(),
		"postgres": run(filepath.Join(pgBin, "postgres"), "--version"),
		"note":     "the harness, the engine, the workers and Postgres all share this one machine",
	}
	if runtime.GOOS == "darwin" {
		env["cpu"] = run("sysctl", "-n", "machdep.cpu.brand_string")
		env["logical_cpus"] = run("sysctl", "-n", "hw.ncpu")
		env["memory_bytes"] = run("sysctl", "-n", "hw.memsize")
		env["os_version"] = run("sw_vers", "-productVersion")
	}
	var syncCommit, sharedBuffers, fsync string
	rig.Pool.QueryRow(context.Background(), `SHOW synchronous_commit`).Scan(&syncCommit)
	rig.Pool.QueryRow(context.Background(), `SHOW shared_buffers`).Scan(&sharedBuffers)
	rig.Pool.QueryRow(context.Background(), `SHOW fsync`).Scan(&fsync)
	env["synchronous_commit"], env["shared_buffers"], env["fsync"] = syncCommit, sharedBuffers, fsync
	env["workers"] = fmt.Sprint(rig.Workers)
	env["engines"], env["db_max_conns_per_engine"] = fmt.Sprint(rig.Engines), fmt.Sprint(rig.DBConns)
	env["proxy_in_front"] = fmt.Sprint(rig.UseLB)
	return env
}

// ---------------------------------------------------------------------------
// Thermal throttling
// ---------------------------------------------------------------------------

var speedLimit = regexp.MustCompile(`CPU_Speed_Limit\s*=\s*(\d+)`)

// cpuSpeedLimit is the share of its full speed (percent) the operating system lets the CPU run
// at. A hot laptop lowers it, and everything that is measured while it is low is slower than the
// same system would be cool. 100 means unthrottled. It is 100 where it cannot be read (not macOS,
// or macOS has nothing to report).
func cpuSpeedLimit() int {
	if runtime.GOOS != "darwin" {
		return 100
	}
	out, err := exec.Command("pmset", "-g", "therm").Output()
	if err != nil {
		return 100
	}
	if m := speedLimit.FindSubmatch(out); m != nil {
		var n int
		fmt.Sscan(string(m[1]), &n)
		return n
	}
	return 100
}

// waitForCool holds a run back until the CPU is no longer throttled, for at most `limit`.
func waitForCool(ctx context.Context, limit time.Duration) int {
	deadline := time.Now().Add(limit)
	waited := false
	for {
		n := cpuSpeedLimit()
		if n >= 100 || time.Now().After(deadline) || ctx.Err() != nil {
			if waited {
				fmt.Printf("   the CPU is at %d%% of its speed limit; starting\n", n)
			}
			return n
		}
		if !waited {
			fmt.Printf("== the CPU is throttled to %d%% (it is hot); waiting for it to cool before starting\n", n)
			waited = true
		}
		select {
		case <-ctx.Done():
		case <-time.After(15 * time.Second):
		}
	}
}

// watchThermal samples the CPU speed limit every few seconds and keeps the lowest value seen.
func watchThermal(ctx context.Context, lowest *atomic.Int32) {
	for {
		n := int32(cpuSpeedLimit())
		for {
			cur := lowest.Load()
			if n >= cur || lowest.CompareAndSwap(cur, n) {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}
