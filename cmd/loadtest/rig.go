package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/skifast/durable-job-engine/internal/store"
)

// proc is a child process in its own process group, so that one kill takes down the
// process and anything it started (Postgres starts a backend per connection).
type proc struct {
	name    string
	bin     string
	args    []string
	env     []string
	logPath string

	cmd    *exec.Cmd
	exited chan struct{}
}

func (p *proc) start() error {
	log, err := os.OpenFile(p.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	p.cmd = exec.Command(p.bin, p.args...)
	p.cmd.Env = append(os.Environ(), p.env...)
	p.cmd.Stdout, p.cmd.Stderr = log, log
	p.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := p.cmd.Start(); err != nil {
		log.Close()
		return fmt.Errorf("start %s: %w", p.name, err)
	}
	p.exited = make(chan struct{})
	go func(cmd *exec.Cmd, exited chan struct{}) {
		cmd.Wait()
		log.Close()
		close(exited)
	}(p.cmd, p.exited)
	return nil
}

func (p *proc) running() bool {
	if p.exited == nil {
		return false
	}
	select {
	case <-p.exited:
		return false
	default:
		return true
	}
}

// kill9 sends SIGKILL to the whole process group and waits for it to be gone.
func (p *proc) kill9() {
	if !p.running() {
		return
	}
	syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	<-p.exited
}

// stop asks the process to shut down cleanly (SIGTERM; Postgres treats SIGTERM as "smart"
// shutdown, so Postgres is stopped with SIGINT, "fast", by stopFast).
func (p *proc) stop(sig syscall.Signal, wait time.Duration) {
	if !p.running() {
		return
	}
	syscall.Kill(p.cmd.Process.Pid, sig)
	select {
	case <-p.exited:
	case <-time.After(wait):
		p.kill9()
	}
}

func (p *proc) signal(sig syscall.Signal) { syscall.Kill(-p.cmd.Process.Pid, sig) }

// Rig is a whole system on this machine: its own Postgres cluster, the engine, and workers.
type Rig struct {
	Dir     string
	PgBin   string
	Workers int

	PgPort     int
	EnginePort int

	pg      *proc
	engine  *proc
	workers []*proc

	Pool *pgxpool.Pool // the harness's own access to the database, for measuring
}

const (
	clientKey = "load-client"
	dbName    = "jobs"
)

func token(name string) string { return "token-" + name }

func (r *Rig) DBURL() string {
	return fmt.Sprintf("postgres://postgres@127.0.0.1:%d/%s?sslmode=disable", r.PgPort, dbName)
}
func (r *Rig) EngineURL() string { return fmt.Sprintf("http://127.0.0.1:%d", r.EnginePort) }

// jobTypes is the engine's job-type file for the tests. Every type uses the defaults
// (8 attempts) except sleep_long, whose time limit is long enough for the lease tests.
const jobTypes = `{"job_types": {
  "noop":       {},
  "sleep":      {},
  "sleep_long": {"handler_time_limit": "10m"}
}}`

// newRig builds the binaries, creates a fresh Postgres cluster with stock settings, loads the
// schema, creates the API keys, and starts the engine and r.Workers workers.
func newRig(repo, dir, pgBin string, workers int) (*Rig, error) {
	r := &Rig{Dir: dir, PgBin: pgBin, Workers: workers, PgPort: 55432, EnginePort: 58080}
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		return nil, err
	}

	for _, b := range []string{"engine", "worker"} {
		cmd := exec.Command("go", "build", "-o", filepath.Join(dir, "bin", b), "./cmd/"+b)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("build %s: %v\n%s", b, err, out)
		}
	}

	// A fresh cluster with the default settings, so the results can be repeated.
	data := filepath.Join(dir, "pgdata")
	if out, err := exec.Command(filepath.Join(pgBin, "initdb"), "-D", data, "-U", "postgres",
		"--auth=trust", "-E", "UTF8", "--locale=C").CombinedOutput(); err != nil {
		return nil, fmt.Errorf("initdb: %v\n%s", err, out)
	}
	r.pg = &proc{
		name: "postgres", bin: filepath.Join(pgBin, "postgres"),
		args: []string{"-D", data, "-p", fmt.Sprint(r.PgPort), "-c", "listen_addresses=127.0.0.1",
			"-c", "unix_socket_directories="}, // TCP only: the scratch path is too long for a socket
		logPath: filepath.Join(dir, "postgres.log"),
	}
	if err := r.startPostgres(); err != nil {
		return nil, err
	}

	ctx := context.Background()
	admin, err := pgxpool.New(ctx, fmt.Sprintf("postgres://postgres@127.0.0.1:%d/postgres?sslmode=disable", r.PgPort))
	if err != nil {
		return nil, err
	}
	_, err = admin.Exec(ctx, "CREATE DATABASE "+dbName+" ENCODING 'UTF8' TEMPLATE template0 LC_COLLATE 'C' LC_CTYPE 'C'")
	admin.Close()
	if err != nil {
		return nil, err
	}
	if r.Pool, err = pgxpool.New(ctx, r.DBURL()); err != nil {
		return nil, err
	}
	for _, f := range []string{"db/schema.sql", "db/invariants.sql"} {
		sql, err := os.ReadFile(filepath.Join(repo, f))
		if err != nil {
			return nil, err
		}
		if _, err := r.Pool.Exec(ctx, string(sql)); err != nil {
			return nil, fmt.Errorf("load %s: %w", f, err)
		}
	}

	keys := map[string]string{clientKey: "client"}
	for i := 1; i <= workers; i++ {
		keys[fmt.Sprintf("worker-%d", i)] = "worker"
	}
	for name, role := range keys {
		if _, err := r.Pool.Exec(ctx, `INSERT INTO api_keys (name, role, key_hash) VALUES ($1, $2, $3)`,
			name, role, store.HashKey(token(name))); err != nil {
			return nil, err
		}
	}

	typesPath := filepath.Join(dir, "job-types.json")
	if err := os.WriteFile(typesPath, []byte(jobTypes), 0o644); err != nil {
		return nil, err
	}
	r.engine = &proc{
		name: "engine", bin: filepath.Join(dir, "bin", "engine"), logPath: filepath.Join(dir, "engine.log"),
		env: []string{"DATABASE_URL=" + r.DBURL(), "JOB_TYPES_FILE=" + typesPath,
			fmt.Sprintf("LISTEN_ADDR=127.0.0.1:%d", r.EnginePort)},
	}
	if err := r.startEngine(); err != nil {
		return nil, err
	}
	for i := 1; i <= workers; i++ {
		w := &proc{
			name: fmt.Sprintf("worker-%d", i), bin: filepath.Join(dir, "bin", "worker"),
			logPath: filepath.Join(dir, fmt.Sprintf("worker-%d.log", i)),
			env:     []string{"WORKER_KEY=" + token(fmt.Sprintf("worker-%d", i)), "ENGINE_URL=" + r.EngineURL()},
		}
		if err := w.start(); err != nil {
			return nil, err
		}
		r.workers = append(r.workers, w)
	}
	return r, nil
}

func (r *Rig) startPostgres() error {
	if err := r.pg.start(); err != nil {
		return err
	}
	deadline := time.Now().Add(60 * time.Second) // crash recovery can take a while
	for time.Now().Before(deadline) {
		if out, err := exec.Command(filepath.Join(r.PgBin, "pg_isready"), "-h", "127.0.0.1", "-p", fmt.Sprint(r.PgPort)).CombinedOutput(); err == nil {
			_ = out
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("postgres did not become ready")
}

func (r *Rig) startEngine() error {
	if err := r.engine.start(); err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		res, err := http.Get(r.EngineURL() + "/healthz")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("engine did not become healthy; see %s", r.engine.logPath)
}

// KillPostgres sends SIGKILL to Postgres and all its backends, like a crash. The
// database recovers from its write-ahead log when it is started again.
func (r *Rig) KillPostgres()          { r.pg.kill9() }
func (r *Rig) RestartPostgres() error { return r.startPostgres() }
func (r *Rig) KillEngine()            { r.engine.kill9() }
func (r *Rig) RestartEngine() error   { return r.startEngine() }

func (r *Rig) KillWorker(i int) { r.workers[i].kill9() }
func (r *Rig) RestartWorker(i int) error {
	return r.workers[i].start()
}

// Close stops everything. The data directory is kept so a failed run can be inspected.
func (r *Rig) Close() {
	for _, w := range r.workers {
		w.stop(syscall.SIGTERM, 40*time.Second)
	}
	if r.engine != nil {
		r.engine.stop(syscall.SIGTERM, 15*time.Second)
	}
	if r.Pool != nil {
		r.Pool.Close()
	}
	if r.pg != nil {
		r.pg.stop(syscall.SIGINT, 30*time.Second) // SIGINT is Postgres's "fast shutdown"
	}
}

// logContains reports whether a process log has text in it (used to check what a worker said).
func logContains(path, text string) bool {
	b, err := os.ReadFile(path)
	return err == nil && bytes.Contains(b, []byte(text))
}
