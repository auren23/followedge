package collector

import (
	"context"
	"sync"
	"time"
)

// Limiter is a token bucket weighted by request cost (GMGN weights: 1 for
// smartmoney/kol, 2 for kline) plus a global cooldown gate: once any caller
// reports a 429, every caller waits until the gate opens. GMGN bans the IP
// and each request during a ban extends it by 5s — the gate exists so the
// pipeline can never do that by accident.
type Limiter struct {
	mu            sync.Mutex
	tokens        float64
	capacity      float64
	rate          float64 // tokens per second
	last          time.Time
	cooldownUntil time.Time
}

func NewLimiter(weightPerSecond float64, burst int) *Limiter {
	return &Limiter{
		tokens:   float64(burst),
		capacity: float64(burst),
		rate:     weightPerSecond,
		last:     time.Now(),
	}
}

// MarkCooldown slams the global gate shut after a 429. resetAt is honored
// when known; otherwise a 30s minimum keeps the retry loop from re-hitting
// the ban window.
func (l *Limiter) MarkCooldown(resetAt time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	until := resetAt
	if until.IsZero() || until.Before(time.Now().Add(30*time.Second)) {
		until = time.Now().Add(30 * time.Second)
	}
	if until.After(l.cooldownUntil) {
		l.cooldownUntil = until
	}
}

// Take blocks until `weight` tokens are available and the cooldown gate is
// open, or ctx is done.
func (l *Limiter) Take(ctx context.Context, weight float64) error {
	if weight <= 0 {
		return nil
	}
	for {
		l.mu.Lock()
		if until := l.cooldownUntil; time.Now().Before(until) {
			wait := until.Sub(time.Now())
			l.mu.Unlock()
			if !sleepCtx(ctx, wait) {
				return ctx.Err()
			}
			continue
		}
		now := time.Now()
		l.tokens += now.Sub(l.last).Seconds() * l.rate
		if l.tokens > l.capacity {
			l.tokens = l.capacity
		}
		l.last = now
		if l.tokens >= weight {
			l.tokens -= weight
			l.mu.Unlock()
			return nil
		}
		wait := time.Duration((weight - l.tokens) / l.rate * float64(time.Second))
		l.mu.Unlock()

		if !sleepCtx(ctx, wait) {
			return ctx.Err()
		}
	}
}
