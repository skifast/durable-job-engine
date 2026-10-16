package api_test

import (
	"net/http"
	"strings"
	"testing"
)

const someID = "6f1c0e5a-6a53-4d6e-9d57-0a2b7f2f6b11"

// A path that exists, called with a method it does not accept, is a 405 in the engine's own
// error shape with an Allow header. (It used to be answered "404 No such endpoint", which was
// wrong: the endpoint exists.) Routing answers come before the key check, so no key is needed.
func TestWrongMethodIsA405WithAllow(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		method, path string
		allow        []string // methods that must be listed
	}{
		{"GET", "/claim", []string{"POST"}},
		{"DELETE", "/jobs", []string{"GET", "POST"}},
		{"PUT", "/jobs/" + someID, []string{"GET"}},
		{"POST", "/jobs/" + someID, []string{"GET"}},
		{"GET", "/jobs/" + someID + "/approve", []string{"POST"}},
		{"GET", "/jobs/" + someID + "/reject", []string{"POST"}},
		{"GET", "/jobs/" + someID + "/redrive", []string{"POST"}},
		{"GET", "/jobs/" + someID + "/heartbeat", []string{"POST"}},
		{"GET", "/jobs/" + someID + "/report", []string{"POST"}},
		{"POST", "/healthz", []string{"GET"}},
		{"POST", "/whoami", []string{"GET"}},
		{"POST", "/ui/", []string{"GET"}},
		{"POST", "/ui", []string{"GET"}},
		{"POST", "/", []string{"GET"}},
		{"OPTIONS", "/jobs", []string{"GET", "POST"}},
		{"HEAD", "/claim", []string{"POST"}}, // HEAD is only accepted where GET is
	}
	for _, c := range cases {
		for _, who := range []string{"", "op1"} { // with no key and with a valid key: the same answer
			r := e.call(c.method, c.path, who, nil, nil)
			if r.status != http.StatusMethodNotAllowed {
				t.Errorf("%s %s (key %q): status %d, want 405: %s", c.method, c.path, who, r.status, r.body)
				continue
			}
			if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("%s %s: Content-Type %q, want JSON", c.method, c.path, ct)
			}
			if c.method != "HEAD" && r.errCode() != "method_not_allowed" {
				t.Errorf("%s %s: error code %q", c.method, c.path, r.errCode())
			}
			allow := r.header.Get("Allow")
			for _, m := range c.allow {
				if !strings.Contains(allow, m) {
					t.Errorf("%s %s: Allow %q lacks %s", c.method, c.path, allow, m)
				}
			}
			if strings.Contains(allow, c.method) {
				t.Errorf("%s %s: Allow %q lists the method that was refused", c.method, c.path, allow)
			}
			if c.method != "HEAD" {
				d := r.json()["error"].(map[string]any)["details"].(map[string]any)
				if d["allow"] != allow {
					t.Errorf("%s %s: details.allow %v differs from the Allow header %q", c.method, c.path, d["allow"], allow)
				}
			}
		}
	}
}

// The methods that are accepted still reach their handlers, and HEAD works where GET does.
func TestRightMethodIsNotA405(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/healthz", 200},
		{"HEAD", "/healthz", 200},
		{"GET", "/whoami", 401}, // reaches the key check
		{"POST", "/claim", 401},
		{"GET", "/jobs", 401},
		{"POST", "/jobs", 401},
		{"GET", "/jobs/" + someID, 401},
		{"POST", "/jobs/" + someID + "/approve", 401},
		{"POST", "/jobs/" + someID + "/report", 401},
		{"GET", "/ui/", 200},
	} {
		r := e.call(c.method, c.path, "", nil, nil)
		if r.status != c.status {
			t.Errorf("%s %s: status %d, want %d", c.method, c.path, r.status, c.status)
		}
	}
	// An unknown path is still a 404 in the error shape, for any method.
	for _, m := range []string{"GET", "POST", "DELETE"} {
		r := e.call(m, "/no/such/endpoint", "op1", nil, nil)
		want(t, m+" unknown path", r.status, 404)
		want(t, m+" unknown path code", r.errCode(), "not_found")
	}
}
