package retry

import (
	"math/rand"
	"testing"
	"time"
)

// With jitter removed the waits must match ADR 004: 2, 4, 8, 16, 32, 60, 60 s.
func TestDelayWithoutJitter(t *testing.T) {
	want := []time.Duration{2, 4, 8, 16, 32, 60, 60, 60}
	for i, w := range want {
		attempt := i + 1
		// Run many times and take the smallest: the lower bound is the unjittered value.
		rnd := rand.New(rand.NewSource(1))
		min := time.Duration(1 << 62)
		for n := 0; n < 1000; n++ {
			if d := Delay(attempt, rnd); d < min {
				min = d
			}
		}
		if min < w*time.Second || min > w*time.Second+w*time.Second/50 {
			t.Errorf("attempt %d: smallest delay %v, want just above %v", attempt, min, w*time.Second)
		}
	}
}

// Jitter stays between 0% and 10% of the delay, and is added after the cap.
func TestJitterBounds(t *testing.T) {
	rnd := rand.New(rand.NewSource(42))
	for attempt := 1; attempt <= 100; attempt++ {
		base := 2 * time.Second << (attempt - 1)
		if attempt > 6 || base > 60*time.Second {
			base = 60 * time.Second
		}
		for n := 0; n < 200; n++ {
			d := Delay(attempt, rnd)
			if d < base || d > base+base/10 {
				t.Fatalf("attempt %d: delay %v outside [%v, %v]", attempt, d, base, base+base/10)
			}
		}
	}
}

// The same seed gives the same delays (a fixed-seed test is possible).
func TestDeterministicWithSeed(t *testing.T) {
	a, b := rand.New(rand.NewSource(7)), rand.New(rand.NewSource(7))
	for attempt := 1; attempt <= 8; attempt++ {
		if Delay(attempt, a) != Delay(attempt, b) {
			t.Fatalf("attempt %d differs for the same seed", attempt)
		}
	}
}

// Seven waits for eight attempts total about 182 s before jitter.
func TestTotalWait(t *testing.T) {
	var sum time.Duration
	for attempt := 1; attempt <= 7; attempt++ {
		sum += Delay(attempt, rand.New(rand.NewSource(int64(attempt)))) // jitter included
	}
	if sum < 182*time.Second || sum > 182*time.Second+182*time.Second/10 {
		t.Errorf("total wait %v, want 182 s to 200.2 s", sum)
	}
}
