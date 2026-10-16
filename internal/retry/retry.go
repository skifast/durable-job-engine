// Package retry holds the retry delay from ADR 004.
//
// It is a plain function: attempt number and a random source in, wait out.
// Nothing here touches the database or the clock, so it is easy to test.
package retry

import (
	"math/rand"
	"time"
)

const (
	baseDelay  = 2 * time.Second  // wait after the first failed attempt
	maxDelay   = 60 * time.Second // the cap, applied before jitter
	jitterFrac = 0.10             // jitter is a random 0-10% of the delay
)

// Delay returns how long to wait before the next attempt.
// failedAttempt is the number of the attempt that just failed (1 for the first).
//
//	delay = min(2s * 2^(failedAttempt-1), 60s) + jitter
//
// Jitter is added after the cap, so a capped delay can reach 66 s.
// Waits without jitter: 2, 4, 8, 16, 32, 60, 60 s.
func Delay(failedAttempt int, rnd *rand.Rand) time.Duration {
	if failedAttempt < 1 {
		failedAttempt = 1 // attempts start at 1; guard against a bad caller
	}
	d := maxDelay
	// Stop doubling once the cap is reached. This also avoids overflow
	// for large attempt numbers (max_attempts can be up to 100).
	if failedAttempt <= 6 {
		d = baseDelay << (failedAttempt - 1)
		if d > maxDelay {
			d = maxDelay
		}
	}
	jitter := time.Duration(rnd.Float64() * jitterFrac * float64(d))
	return d + jitter
}
