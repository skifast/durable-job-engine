package handlers_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/skifast/durable-job-engine/examples/handlers"
)

// Every job type in examples/demo-job-types.json must have a handler. A type without one would
// be claimed by a worker and reported as "no handler for type X" and retried (ADR 014), so a demo
// that submits it would never finish. (needs_approval was once in the file without a handler.)
func TestEveryDemoJobTypeHasAHandler(t *testing.T) {
	raw, err := os.ReadFile("../demo-job-types.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		JobTypes map[string]json.RawMessage `json:"job_types"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.JobTypes) == 0 {
		t.Fatal("no job types found in the file")
	}
	all := handlers.All()
	for name := range file.JobTypes {
		if _, ok := all[name]; !ok {
			t.Errorf("job type %q in demo-job-types.json has no handler in handlers.All()", name)
		}
	}
}
