package collector

import (
	"context"
	"testing"
	"time"
)

func TestDedupTTL(t *testing.T) {
	d := NewDedup(50*time.Millisecond, 100)
	if d.Seen("a") {
		t.Fatal("first Seen should return false")
	}
	if !d.Seen("a") {
		t.Fatal("second Seen should return true")
	}
	time.Sleep(60 * time.Millisecond)
	if d.Seen("a") {
		t.Fatal("after TTL, Seen should be false again")
	}
}

func TestLimiterRate(t *testing.T) {
	l := NewLimiter(10, 5) // 10 tokens/s, burst 5
	ctx := context.Background()
	start := time.Now()
	// burst: 5 tokens available instantly
	for i := 0; i < 5; i++ {
		if err := l.Take(ctx, 1); err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(start)
	if elapsed > 300*time.Millisecond {
		t.Fatalf("burst took %v, want instant", elapsed)
	}
	// next token must wait ~100ms at rate 10/s
	start = time.Now()
	if err := l.Take(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 50*time.Millisecond || d > 500*time.Millisecond {
		t.Fatalf("refill wait = %v, want ~100ms", d)
	}
}

func TestLimiterCancellation(t *testing.T) {
	l := NewLimiter(0.01, 1) // one initial token, then ~empty bucket
	ctx := context.Background()
	if err := l.Take(ctx, 1); err != nil {
		t.Fatal(err) // consumes the initial token
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := l.Take(ctx, 1); err == nil {
		t.Fatal("expected context cancellation error")
	}
}

func TestCooldownGateFreezesAllCallers(t *testing.T) {
	l := NewLimiter(10, 5)
	l.MarkCooldown(time.Now().Add(45 * time.Second)) // 429 reset far in the future
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := l.Take(ctx, 1); err == nil {
		t.Fatal("take during cooldown must block until ctx deadline")
	}

	// unknown/zero reset must still freeze for the minimum window
	l2 := NewLimiter(10, 5)
	l2.MarkCooldown(time.Time{})
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	if err := l2.Take(ctx2, 1); err == nil {
		t.Fatal("unknown reset must still freeze for the minimum window")
	}
}
