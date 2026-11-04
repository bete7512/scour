package scour

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLimiter(t *testing.T) {
	// 1000/s with a burst of 100: 300 tokens need at least 200ms.
	l := newLimiter(1000)
	start := time.Now()
	for range 300 {
		if err := l.wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed < 190*time.Millisecond {
		t.Errorf("300 tokens at 1000/s took %v, want at least 200ms", elapsed)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("300 tokens at 1000/s took %v, far more than the 200ms expected", elapsed)
	}
}

func TestLimiterCancel(t *testing.T) {
	l := newLimiter(1) // one token, then a second per token
	if err := l.wait(ctx); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := l.wait(cctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("cancelled wait took %v", elapsed)
	}
}
