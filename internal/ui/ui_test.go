package ui_test

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/skifast/durable-job-engine/internal/ui"
)

func get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	// The handler is mounted at /ui/ by the API; do the same here.
	mux := http.NewServeMux()
	mux.Handle("GET /ui/", ui.Handler())
	mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

func TestEveryResponseCarriesTheSecurityHeaders(t *testing.T) {
	for _, path := range []string{"/ui/", "/ui/app.mjs", "/ui/style.css", "/ui/no-such-file"} {
		rec := get(t, path)
		h := rec.Header()
		csp := h.Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'none'", "script-src 'self'", "style-src 'self'", "connect-src 'self'", "frame-ancestors 'none'", "base-uri 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: CSP lacks %q: %s", path, want, csp)
			}
		}
		for _, bad := range []string{"unsafe-inline", "unsafe-eval", "http:", "https:", "*"} {
			if strings.Contains(csp, bad) {
				t.Errorf("%s: CSP must not contain %q: %s", path, bad, csp)
			}
		}
		if h.Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s: X-Frame-Options %q", path, h.Get("X-Frame-Options"))
		}
		if h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options %q", path, h.Get("X-Content-Type-Options"))
		}
		// Go's http.Error, used for the 404, removes Cache-Control. A cached 404 does no harm, so
		// only the files that exist must say no-store (a changed page is never served stale).
		if rec.Code == 200 && h.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: Cache-Control %q", path, h.Get("Cache-Control"))
		}
	}
}

func TestFilesAreServedWithTheRightTypes(t *testing.T) {
	cases := map[string]string{
		"/ui/":           "text/html",
		"/ui/app.mjs":    "text/javascript", // a browser refuses a module served as anything else
		"/ui/client.mjs": "text/javascript",
		"/ui/format.mjs": "text/javascript",
		"/ui/style.css":  "text/css",
	}
	for path, want := range cases {
		rec := get(t, path)
		if rec.Code != 200 {
			t.Errorf("%s: status %d", path, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, want) {
			t.Errorf("%s: Content-Type %q, want %s", path, ct, want)
		}
	}
	if rec := get(t, "/ui/no-such-file"); rec.Code != 404 {
		t.Errorf("missing file: status %d", rec.Code)
	}
}

// The page must not contain anything that turns text into markup or runs text as code. Job
// payloads, results and error messages are written by other people (ADR 015, decision 3).
func TestPageScriptsHaveNoHTMLInjectionSinks(t *testing.T) {
	forbidden := []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "createContextualFragment",
		"eval(", "new Function", "setTimeout(\"", "setInterval(\"", "javascript:", "srcdoc", "DOMParser"}
	n := 0
	err := fs.WalkDir(ui.Files(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".mjs") {
			return err
		}
		n++
		b, err := fs.ReadFile(ui.Files(), path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue // comments may talk about these names
			}
			for _, f := range forbidden {
				if strings.Contains(line, f) {
					t.Errorf("%s line %d contains %q: %s", path, i+1, f, strings.TrimSpace(line))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 3 {
		t.Errorf("only %d scripts found; the guard is not looking at the page", n)
	}
}

// The policy forbids inline script, inline style and event-handler attributes, so the HTML must have none.
func TestHTMLHasNothingInline(t *testing.T) {
	b, err := fs.ReadFile(ui.Files(), "index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	if regexp.MustCompile(`(?i)<script(\s[^>]*)?>`).FindAllString(html, -1) == nil {
		t.Fatal("no script tag found")
	}
	for _, tag := range regexp.MustCompile(`(?i)<script(\s[^>]*)?>`).FindAllString(html, -1) {
		if !strings.Contains(tag, `src="`) {
			t.Errorf("inline script: %s", tag)
		}
	}
	if regexp.MustCompile(`(?i)<style[\s>]`).MatchString(html) {
		t.Error("inline <style> block")
	}
	if regexp.MustCompile(`(?i)\sstyle\s*=`).MatchString(html) {
		t.Error("inline style attribute")
	}
	if regexp.MustCompile(`(?i)\son[a-z]+\s*=`).MatchString(html) {
		t.Error("event-handler attribute")
	}
	// Every file the HTML points at exists, and none is on another site.
	for _, m := range regexp.MustCompile(`(?:src|href)="([^"]+)"`).FindAllStringSubmatch(html, -1) {
		ref := m[1]
		if strings.Contains(ref, "://") || strings.HasPrefix(ref, "//") {
			t.Errorf("reference to another site: %s", ref)
			continue
		}
		if _, err := fs.Stat(ui.Files(), ref); err != nil {
			t.Errorf("index.html points at %s, which is not in the page: %v", ref, err)
		}
	}
}
