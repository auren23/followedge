// Package collector polls trade sources on a schedule, dedups, persists and
// hands new events to the downstream pipeline. It decides nothing about
// whether a token is worth trading.
package collector

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"github.com/auren23/followedge/internal/domain"
	"github.com/auren23/followedge/internal/source/gmgn"
	"github.com/auren23/followedge/internal/storage"
)

// Source is any trade feed that yields normalized events.
type Source interface {
	Name() string
	Weight() float64 // limiter cost of one poll
	Poll(ctx context.Context) ([]domain.TradeEvent, error)
}

// GMGNSource adapts a GMGN feed (smartmoney or kol) to a Source.
type GMGNSource struct {
	NameValue  string
	WalletType domain.WalletType
	Client     *gmgn.Client
	Chain      string
	Limit      int
}

func (s *GMGNSource) Name() string    { return s.NameValue }
func (s *GMGNSource) Weight() float64 { return 1 }

func (s *GMGNSource) Poll(ctx context.Context) ([]domain.TradeEvent, error) {
	receivedAt := time.Now().UTC()
	var items []gmgn.TradeItem
	var err error
	if s.WalletType == domain.WalletKOL {
		items, err = s.Client.KOL(ctx, s.Chain, s.Limit)
	} else {
		items, err = s.Client.SmartMoney(ctx, s.Chain, s.Limit)
	}
	if err != nil {
		return nil, err
	}
	out := make([]domain.TradeEvent, 0, len(items))
	for _, it := range items {
		e, err := gmgn.NormalizeTrade(it, s.Name(), s.WalletType, receivedAt)
		if err != nil {
			slog.Warn("normalize failed", "source", s.Name(), "err", err)
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// Collector runs one Source on a fixed schedule with backoff.
type Collector struct {
	Source   Source
	Interval time.Duration

	Store   *storage.Store
	Limiter *Limiter
	Dedup   *Dedup

	// OnEvent is called (in order) for every freshly created event.
	OnEvent func(ctx context.Context, e domain.TradeEvent) error

	// Print renders the human-readable trade line.
	Print func(e domain.TradeEvent)
}

// PollOnce does a single poll cycle; returns the number of new events.
func (c *Collector) PollOnce(ctx context.Context) (int, error) {
	if err := c.Limiter.Take(ctx, c.Source.Weight()); err != nil {
		return 0, err
	}
	events, err := c.Source.Poll(ctx)
	if err != nil {
		return 0, err
	}
	newEvents := 0
	for _, e := range events {
		if c.Dedup.Seen(e.ID) {
			continue
		}
		created, err := c.Store.InsertEvent(e)
		if err != nil {
			return newEvents, fmt.Errorf("%s insert: %w", c.Source.Name(), err)
		}
		if !created {
			continue // duplicate across restarts: DB is the source of truth
		}
		newEvents++
		if c.OnEvent != nil {
			if err := c.OnEvent(ctx, e); err != nil {
				slog.Warn("pipeline event failed", "source", c.Source.Name(), "event", e.ID, "err", err)
			}
		}
		if c.Print != nil {
			c.Print(e)
		}
	}
	return newEvents, nil
}

// Run polls forever: fixed interval + jitter, exponential backoff on errors,
// and a cooldown sleep on 429 (respected ResetAt when known).
func (c *Collector) Run(ctx context.Context) {
	log := slog.With("source", c.Source.Name())
	backoff := time.Second
	for {
		start := time.Now()
		_, err := c.PollOnce(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			sleep := backoff
			if rl := asRateLimit(err); rl != nil {
				sleep = 5 * time.Second
				if !rl.ResetAt.IsZero() {
					if d := time.Until(rl.ResetAt); d > 0 && d < 5*time.Minute {
						sleep = d
					}
				}
				// freeze every caller (incl. the markout worker) past the reset
				c.Limiter.MarkCooldown(rl.ResetAt)
				log.Warn("rate limited, cooling down", "sleep", sleep.Round(time.Second))
			} else {
				log.Warn("poll failed", "err", err, "retry_in", sleep.Round(time.Second))
			}
			if !sleepCtx(ctx, sleep) {
				return
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second

		// poll interval minus elapsed, plus jitter
		wait := c.Interval - time.Since(start) + time.Duration(rand.Float64()*0.1*float64(c.Interval))
		if wait < 100*time.Millisecond {
			wait = 100 * time.Millisecond
		}
		if !sleepCtx(ctx, wait) {
			return
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// asRateLimit unwraps a gmgn rate-limit error.
func asRateLimit(err error) *gmgn.RateLimitError {
	if rl, ok := err.(*gmgn.RateLimitError); ok {
		return rl
	}
	return nil
}
