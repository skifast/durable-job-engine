package config

import (
	"strings"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte(`{"job_types":{"send_email":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	got := c.JobTypes["send_email"]
	want := JobType{false, 24 * time.Hour, 60 * time.Second, 8}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestExplicitValues(t *testing.T) {
	c, err := Parse([]byte(`{"job_types":{"pay":{"needs_approval":true,"approval_window":"2h","handler_time_limit":"5s","max_attempts":3}}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := JobType{true, 2 * time.Hour, 5 * time.Second, 3}
	if got := c.JobTypes["pay"]; got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestRejects(t *testing.T) {
	cases := map[string]string{
		"empty":            `{"job_types":{}}`,
		"typo in field":    `{"job_types":{"a":{"max_attemps":3}}}`,
		"max_attempts 0":   `{"job_types":{"a":{"max_attempts":0}}}`,
		"max_attempts 101": `{"job_types":{"a":{"max_attempts":101}}}`,
		"bad duration":     `{"job_types":{"a":{"handler_time_limit":"soon"}}}`,
		"zero duration":    `{"job_types":{"a":{"approval_window":"0s"}}}`,
		"empty name":       `{"job_types":{"":{}}}`,
		"not json":         `job_types: nope`,
	}
	for name, in := range cases {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: expected an error", name)
		} else if !strings.HasPrefix(err.Error(), "config:") {
			t.Errorf("%s: error %q should start with 'config:'", name, err)
		}
	}
}

func TestParseMaxConns(t *testing.T) {
	if DefaultMaxConns != 10 {
		t.Errorf("the default is documented as 10 (docs and ADR 016), not %d", DefaultMaxConns)
	}
	if n, err := ParseMaxConns(""); err != nil || n != DefaultMaxConns {
		t.Errorf("empty: got %d, %v; want the default", n, err)
	}
	for in, want := range map[string]int32{"1": 1, "10": 10, "250": 250, "1000": 1000} {
		if n, err := ParseMaxConns(in); err != nil || n != want {
			t.Errorf("%q: got %d, %v; want %d", in, n, err, want)
		}
	}
	for _, in := range []string{"0", "-3", "1001", "ten", "5.5", " 5", "5 ", "1e2", "0x10"} {
		if _, err := ParseMaxConns(in); err == nil {
			t.Errorf("%q should be refused", in)
		}
	}
}
