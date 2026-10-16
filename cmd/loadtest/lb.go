package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// LB is a small round-robin proxy for the tests. It stands in for the load balancer that would sit
// in front of several engines in a real deployment. It is a test stand-in, not a production
// load balancer, and, like any single load balancer, it is itself a single point of failure
// (ADR 016).
//
// What it does: it sends each request to the next engine that passed its last health check
// (GET /healthz); it takes an engine out the moment a connection to it is refused or its check
// fails, and puts it back when the check passes again. If a connection to an engine is refused
// (nothing was sent), it tries the next engine.
type LB struct {
	listen   string
	backends []*backend
	srv      *http.Server
	next     atomic.Uint64
	probe    *http.Client

	errors atomic.Int64 // requests the proxy itself had to fail (no engine could take them)

	mu     sync.Mutex
	events []event // engines going out of and back into service
	start  time.Time
	stop   chan struct{}
}

type backend struct {
	url      *url.URL
	name     string
	healthy  atomic.Bool
	requests atomic.Int64
}

var errNoEngine = errors.New("no engine is in service")

func newLB(listen string, engineURLs []string) (*LB, error) {
	l := &LB{listen: listen, start: time.Now(), stop: make(chan struct{}),
		probe: &http.Client{Timeout: 3 * time.Second}}
	for i, u := range engineURLs {
		pu, err := url.Parse(u)
		if err != nil {
			return nil, err
		}
		b := &backend{url: pu, name: fmt.Sprintf("engine-%d", i+1)}
		b.healthy.Store(true)
		l.backends = append(l.backends, b)
	}
	return l, nil
}

func (l *LB) note(what, detail string) {
	l.mu.Lock()
	l.events = append(l.events, event{time.Since(l.start).Seconds(), what, detail})
	l.mu.Unlock()
}

// pick returns the next in-service engine that has not been tried for this request.
func (l *LB) pick(tried map[*backend]bool) *backend {
	n := uint64(len(l.backends))
	start := l.next.Add(1)
	for i := uint64(0); i < n; i++ {
		b := l.backends[(start+i)%n]
		if b.healthy.Load() && !tried[b] {
			return b
		}
	}
	return nil
}

// lbTransport tries an engine and, if the connection is refused (so nothing was sent), the next.
type lbTransport struct {
	lb   *LB
	base *http.Transport
}

func (t *lbTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tried := map[*backend]bool{}
	for range t.lb.backends {
		b := t.lb.pick(tried)
		if b == nil {
			break
		}
		tried[b] = true
		out := req.Clone(req.Context())
		out.URL.Scheme, out.URL.Host, out.Host = "http", b.url.Host, b.url.Host
		b.requests.Add(1)
		resp, err := t.base.RoundTrip(out)
		if err == nil {
			return resp, nil
		}
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			if b.healthy.CompareAndSwap(true, false) {
				t.lb.note("engine out of service", b.name+": connection refused")
			}
			continue // nothing was sent to it, so another engine can take the request
		}
		return nil, err
	}
	return nil, errNoEngine
}

func (l *LB) Start() error {
	ln, err := net.Listen("tcp", l.listen)
	if err != nil {
		return err
	}
	transport := &lbTransport{lb: l, base: &http.Transport{MaxIdleConnsPerHost: 512, IdleConnTimeout: 30 * time.Second}}
	proxy := &httputil.ReverseProxy{
		Director:  func(*http.Request) {}, // the transport picks the engine
		Transport: transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			l.errors.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, `{"error":{"code":"bad_gateway","message":"The load balancer could not reach an engine."}}`)
		},
	}
	l.srv = &http.Server{Handler: proxy}
	go l.srv.Serve(ln)
	go l.healthLoop()
	return nil
}

// healthLoop checks every engine's /healthz four times a second. The engine's own check gives up
// after 2 s, so the probe waits 3 s.
func (l *LB) healthLoop() {
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
		}
		var wg sync.WaitGroup
		for _, b := range l.backends {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				req, _ := http.NewRequestWithContext(ctx, "GET", b.url.String()+"/healthz", nil)
				res, err := l.probe.Do(req)
				ok := err == nil && res.StatusCode == 200
				if res != nil {
					res.Body.Close()
				}
				if b.healthy.Swap(ok) != ok {
					if ok {
						l.note("engine back in service", b.name)
					} else {
						l.note("engine out of service", b.name+": health check failed")
					}
				}
			}()
		}
		wg.Wait()
	}
}

func (l *LB) Stop() {
	select {
	case <-l.stop:
	default:
		close(l.stop)
	}
	if l.srv != nil {
		l.srv.Close()
	}
}

// Requests is how many requests each engine was sent.
func (l *LB) Requests() map[string]int64 {
	m := map[string]int64{}
	for _, b := range l.backends {
		m[b.name] = b.requests.Load()
	}
	return m
}

func (l *LB) Events() []event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]event(nil), l.events...)
}
