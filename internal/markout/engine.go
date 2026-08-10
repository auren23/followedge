// Package markout samples forward returns of every trade at fixed horizons.
// These markouts are the raw material for all EV research: copyability,
// chase decay, wallet attribution.
//
// Price source: GMGN 30s klines (the finest resolution the public API
// offers). The observed price for horizon H is the close of the first candle
// that opens at/after TradeTime+H. Sub-30s horizons therefore need a
// different price source and are not supported in v0.1.
package markout

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/auren23/followedge/internal/source/gmgn"
	"github.com/auren23/followedge/internal/storage"
)

// ParseHorizons converts config strings ("30s", "5m", "1h") to durations.
func ParseHorizons(specs []string) ([]time.Duration, error) {
	out := make([]time.Duration, 0, len(specs))
	for _, s := range specs {
		d, err := time.ParseDuration(s)
		if err != nil {
			return nil, fmt.Errorf("bad horizon %q: %w", s, err)
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// klineFetcher is the price source; *gmgn.Client satisfies it, and tests use
// a fake to keep the sampler deterministic.
type klineFetcher interface {
	Kline(ctx context.Context, chain, address, resolution string, from, to time.Time) ([]gmgn.Candle, error)
}

// weightLimiter throttles requests; *collector.Limiter satisfies it. Markouts
// share the collector's bucket (kline weight 2) and its cooldown gate — a
// 429 must freeze the whole pipeline, not just this worker, or the ban gets
// extended by every retry.
type weightLimiter interface {
	Take(ctx context.Context, weight float64) error
	MarkCooldown(resetAt time.Time)
}

// Engine samples due markouts in batches: one kline request per token covers
// every pending (event, horizon) of that token.
type Engine struct {
	store    *storage.Store
	client   klineFetcher
	limiter  weightLimiter
	chain    string
	res      string // kline resolution, e.g. "30s"
	grace    time.Duration
	horizons []time.Duration
	log      *slog.Logger

	// tokens whose kline came back empty (dead / not trading): retry no
	// sooner than this. Prevents stale rows from burning the tick budget.
	skipUntil map[string]time.Time
}

func NewEngine(store *storage.Store, client *gmgn.Client, limiter weightLimiter,
	chain, resolution string, grace time.Duration, horizons []time.Duration) *Engine {
	return &Engine{store: store, client: client, limiter: limiter, chain: chain,
		res: resolution, grace: grace, horizons: horizons, log: slog.With("pkg", "markout"),
		skipUntil: map[string]time.Time{}}
}

func (e *Engine) Horizons() []time.Duration { return e.horizons }

// Run ticks forever: sample everything that became due since the last tick.
func (e *Engine) Run(ctx context.Context, tick time.Duration) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(tick):
			if err := e.SampleDue(ctx); err != nil {
				e.log.Warn("sample pass failed", "err", err)
			}
		}
	}
}

// SampleDue fills every due markout with the first available candle close at
// or after its due time.
func (e *Engine) SampleDue(ctx context.Context) error {
	due, err := e.store.DueMarkouts(e.grace, time.Now().UTC(), 5000)
	if err != nil {
		return err
	}
	if len(due) == 0 {
		return nil
	}

	// kline requests per tick — a batch of stale events must not spike the API.
	const maxTokensPerTick = 25

	// group by token, keep per-markout due times
	type target struct {
		due       time.Time
		event     string
		kind      string
		horizon   time.Duration
		basePrice float64
		baseMs    int64
	}
	byToken := map[string][]target{}
	earliest := map[string]time.Time{}
	for _, d := range due {
		byToken[d.Token] = append(byToken[d.Token], target{
			d.DueAt, d.EventID, d.Kind, d.Horizon, d.BasePrice, d.BaseMs,
		})
		if t, ok := earliest[d.Token]; !ok || d.DueAt.Before(t) {
			earliest[d.Token] = d.DueAt
		}
	}

	now := time.Now().UTC()
	tokensDone := 0
	for token, targets := range byToken {
		if tokensDone >= maxTokensPerTick {
			break // rest retried next tick
		}
		if skip := e.skipUntil[token]; now.Before(skip) {
			continue // dead token, backoff not elapsed
		}
		if err := e.limiter.Take(ctx, 2); err != nil { // kline weight = 2
			return err
		}
		candles, err := e.client.Kline(ctx, e.chain, token, e.res, earliest[token].Add(-30*time.Second), now)
		if err != nil {
			if rl, ok := err.(*gmgn.RateLimitError); ok {
				// freeze the entire pipeline until the ban lifts; do NOT retry
				// this token this tick (every retry inside the window is +5s)
				e.limiter.MarkCooldown(rl.ResetAt)
				e.log.Warn("kline rate limited, cooling down", "token", token,
					"reset", rl.ResetAt.Format(time.RFC3339))
				break // gate is shut; no point fetching more tokens this tick
			} else {
				e.log.Warn("kline fetch failed", "token", token, "err", err)
			}
			continue
		}
		tokensDone++
		if len(candles) == 0 {
			// no kline at all — token likely dead; don't burn budget on it
			// again for a while
			e.skipUntil[token] = now.Add(15 * time.Minute)
			continue
		}
		// candle open times in unix seconds, ascending
		times := make([]int64, len(candles))
		closes := make([]float64, len(candles))
		for i, c := range candles {
			times[i] = c.Time / 1000
			closes[i], err = strconv.ParseFloat(c.Close, 64)
			if err != nil {
				closes[i] = 0
			}
		}
		for _, t := range targets {
			// follower rows need their ReceivedAt entry price first: the close
			// of the candle IN PROGRESS at base_ms (what you could actually
			// have traded at the moment you learned about the trade).
			if t.kind == storage.MarkoutFollower && t.basePrice == 0 {
				if p, ok := lastCloseAtOrBefore(times, closes, t.baseMs); ok && p > 0 {
					if err := e.store.SetEntryPrice(t.event, t.kind, t.horizon, p); err != nil {
						e.log.Warn("set entry price failed", "event", t.event, "err", err)
					}
					t.basePrice = p
				}
			}
			if t.basePrice <= 0 {
				continue // entry unknown; horizon return would be meaningless
			}
			obs, ok := firstCloseAtOrAfter(times, closes, t.due.Unix())
			if !ok || obs <= 0 {
				continue // no candle yet; next tick retries
			}
			if err := e.store.FillMarkout(t.event, t.kind, t.horizon, obs); err != nil {
				e.log.Warn("fill markout failed", "event", t.event, "horizon", t.horizon, "err", err)
			}
		}
	}
	return nil
}

// firstCloseAtOrAfter returns the close of the first candle opening at or
// after ts. times must be ascending.
func firstCloseAtOrAfter(times []int64, closes []float64, ts int64) (float64, bool) {
	i := sort.Search(len(times), func(i int) bool { return times[i] >= ts })
	if i == len(times) {
		return 0, false
	}
	return closes[i], closes[i] > 0
}

// lastCloseAtOrBefore returns the close of the last candle opening at or
// before ts — the candle in progress at ts. Used for follower entry prices:
// the price you could actually have traded when the trade reached you.
func lastCloseAtOrBefore(times []int64, closes []float64, ts int64) (float64, bool) {
	i := sort.Search(len(times), func(i int) bool { return times[i] > ts })
	if i == 0 {
		return 0, false
	}
	return closes[i-1], closes[i-1] > 0
}
