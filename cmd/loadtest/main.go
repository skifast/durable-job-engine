// Command loadtest runs the load, soak and chaos tests of spec sections 3, 8 and 12.
//
// It starts its own Postgres cluster (stock settings, in a scratch directory), the engine
// and the workers, all on this machine, then drives load through the public API and reads
// the results back from the database. Run it from the repository root:
//
//	go run ./cmd/loadtest throughput -dir /tmp/lt -out results/throughput.json
//
// Scenarios: throughput, max-throughput, start-latency, backlog, soak, chaos-worker,
// chaos-engine, chaos-postgres, lease-recovery, pause-worker.
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
	"runtime"
	"strings"
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
	case "chaos-worker", "chaos-engine", "chaos-postgres":
		c.chaosKind = strings.TrimPrefix(name, "chaos-")
		switch c.chaosKind {
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
	rig, err := newRig(repo, *dir, *pgBin, needWorkers)
	if err != nil {
		fatal(err)
	}
	defer rig.Close()

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
	case "chaos-worker", "chaos-engine", "chaos-postgres":
		rep, err = chaos(ctx, rig, c)
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
	return env
}
