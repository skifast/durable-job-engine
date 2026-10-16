package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Errors the client turns into values the worker acts on.
var (
	// ErrLeaseLost: the engine answered 409 lease_lost. This worker no longer holds the job.
	ErrLeaseLost = errors.New("lease lost")
	// ErrResultTooLarge: the engine answered 413 result_too_large to a report.
	ErrResultTooLarge = errors.New("result too large")
	// ErrRejectedKey: the engine answered 401 or 403. Polling again will not help.
	ErrRejectedKey = errors.New("the engine rejected this worker's API key")
)

// StatusError is any other answer that is not a success.
type StatusError struct {
	Status  int
	Code    string
	Message string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("engine answered %d %s: %s", e.Status, e.Code, e.Message)
}

// retryable is true for answers where asking again can work (the engine or its database
// is down). A 4xx other than the ones handled above means the request itself is wrong.
func (e *StatusError) retryable() bool { return e.Status >= 500 }

// Client calls the engine's worker endpoints (docs/api.md).
type Client struct {
	BaseURL string       // for example http://localhost:8080
	Key     string       // this worker's API key; its name becomes lease_owner
	HTTP    *http.Client // nil uses a client with a 10 s timeout per request (ADR 014)
}

// DefaultHTTPTimeout is the timeout of every request the default client makes.
const DefaultHTTPTimeout = 10 * time.Second

// Item is a claimed job, as /claim returns it.
type Item struct {
	ID                 string          `json:"id"`
	Type               string          `json:"type"`
	Payload            json.RawMessage `json:"payload"`
	Attempt            int             `json:"attempt"`
	HandlerTimeLimitMS int64           `json:"handler_time_limit_ms"`
}

// Claim asks for the oldest due job. It returns nil, nil when none is due.
func (c *Client) Claim(ctx context.Context) (*Item, error) {
	status, body, err := c.do(ctx, "/claim", nil)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusNoContent:
		return nil, nil
	case http.StatusOK:
		var it Item
		if err := json.Unmarshal(body, &it); err != nil {
			return nil, fmt.Errorf("claim: unreadable answer: %w", err)
		}
		return &it, nil
	}
	return nil, mapStatus(status, body)
}

// Heartbeat renews the lease. It returns ErrLeaseLost if the fence did not match.
func (c *Client) Heartbeat(ctx context.Context, id string, attempt int) error {
	status, body, err := c.do(ctx, "/jobs/"+id+"/heartbeat", map[string]any{"attempt": attempt})
	if err != nil {
		return err
	}
	if status == http.StatusOK {
		return nil
	}
	return mapStatus(status, body)
}

// Report sends an outcome: "success", "retryable", "permanent" or "unclassified".
// result is used with success only; message with the failures.
func (c *Client) Report(ctx context.Context, id string, attempt int, outcome string, result json.RawMessage, message string) error {
	req := map[string]any{"attempt": attempt, "outcome": outcome}
	if len(result) > 0 {
		req["result"] = result
	}
	if message != "" {
		req["message"] = message
	}
	status, body, err := c.do(ctx, "/jobs/"+id+"/report", req)
	if err != nil {
		return err
	}
	if status == http.StatusOK { // includes replayed: true, which is also a success
		return nil
	}
	return mapStatus(status, body)
}

func (c *Client) do(ctx context.Context, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = defaultHTTP
	}
	res, err := hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return res.StatusCode, b, err
}

var defaultHTTP = &http.Client{Timeout: DefaultHTTPTimeout}

// mapStatus turns a non-success answer into the error the worker acts on.
func mapStatus(status int, body []byte) error {
	var eb struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	json.Unmarshal(body, &eb) // a body that is not our error shape leaves the fields empty
	code := eb.Error.Code
	switch {
	case status == http.StatusConflict && code == "lease_lost":
		return ErrLeaseLost
	case status == http.StatusRequestEntityTooLarge && code == "result_too_large":
		return ErrResultTooLarge
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ErrRejectedKey
	}
	return &StatusError{Status: status, Code: code, Message: eb.Error.Message}
}
