package scour

import (
	"context"
	"sync"
	"time"
)

// A limiter is a token bucket. Every metadata operation takes one token.
// The bucket refills at rate per second and holds a tenth of a second's
// worth, so a burst after a pause stays short.
type limiter struct {
	mu     sync.Mutex
	rate   float64 // tokens per second
	burst  float64
	tokens float64
	last   time.Time
}

func newLimiter(opsPerSec int) *limiter {
	rate := float64(opsPerSec)
	burst := max(rate/10, 1)
	return &limiter{rate: rate, burst: burst, tokens: burst, last: time.Now()}
}

// wait takes a token, sleeping until it is due. Tokens are handed out in
// call order: each caller takes on the debt left by the ones before it and
// sleeps for its share. It returns early with ctx's error if ctx is done.
func (l *limiter) wait(ctx context.Context) error {
	l.mu.Lock()
	now := time.Now()
	l.tokens = min(l.burst, l.tokens+now.Sub(l.last).Seconds()*l.rate)
	l.last = now
	l.tokens--
	var delay time.Duration
	if l.tokens < 0 {
		delay = time.Duration(-l.tokens / l.rate * float64(time.Second))
	}
	l.mu.Unlock()

	if delay == 0 {
		return nil
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
