package api_test

import (
	"net/http"
	"strings"
	"testing"
)

func TestWhoami(t *testing.T) {
	e := newEnv(t)
	for name, role := range map[string]string{"alice": "client", "w1": "worker", "op1": "operator"} {
		r := e.call("GET", "/whoami", name, nil, nil)
		want(t, name+" status", r.status, 200)
		want(t, name+" name", r.json()["name"], name)
		want(t, name+" role", r.json()["role"], role)
	}
	for _, who := range []string{"", "raw:not-a-key"} {
		r := e.call("GET", "/whoami", who, nil, nil)
		want(t, "no key "+who, r.status, 401)
		want(t, "code", r.errCode(), "unauthorized")
	}
}

// The page's files need no key; the root and /ui send a browser to the page.
func TestOperatorPageIsServedWithoutAKey(t *testing.T) {
	e := newEnv(t)
	r := e.call("GET", "/ui/", "", nil, nil)
	want(t, "status", r.status, 200)
	if !strings.Contains(string(r.body), "operator") {
		t.Errorf("not the operator page: %.80s", r.body)
	}
	if !strings.Contains(r.header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Errorf("CSP missing on the page: %q", r.header.Get("Content-Security-Policy"))
	}
	want(t, "script", e.call("GET", "/ui/app.mjs", "", nil, nil).status, 200)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, path := range []string{"/", "/ui"} {
		res, err := client.Get(e.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		want(t, path+" status", res.StatusCode, http.StatusFound)
		want(t, path+" location", res.Header.Get("Location"), "/ui/")
	}

	// Other paths are still the engine's JSON error, not the page.
	r = e.call("GET", "/nothing-here", "", nil, nil)
	want(t, "unknown path", r.status, 404)
	want(t, "unknown path code", r.errCode(), "not_found")
}
