// Package markout samples forward returns of every trade at fixed horizons.
// These markouts are the raw material for all EV research: copyability,
// chase decay, wallet attribution.
//
// Price source: GMGN 30s klines (the finest resolution the public API
// offers). The observed price for horizon H is the close of the first candle
// that CLOSES at/after TradeTime+H (lag bounded by one resolution). Sub-30s
// horizons therefore need a different price source and are not supported in
// v0.1.
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

	// tokens whose kline came back empty: retry no sooner than this, with
	// an escalating backoff (15m → 1h). Prevents stale rows from burning
	// the tick budget while still giving dead-looking tokens another chance
	// — an empty GMGN kline is measurement uncertainty, not proof of death.
	skipUntil   map[string]time.Time
	emptyStreak map[string]int

	resSecs int64 // kline resolution in seconds (for point-in-time entry bounds)
}

func NewEngine(store *storage.Store, client *gmgn.Client, limiter weightLimiter,
	chain, resolution string, grace time.Duration, horizons []time.Duration) *Engine {
	d, err := time.ParseDuration(resolution)
	resSecs := int64(30)
	if err == nil {
		resSecs = int64(d.Seconds())
	}
	return &Engine{store: store, client: client, limiter: limiter, chain: chain,
		res: resolution, grace: grace, horizons: horizons, log: slog.With("pkg", "markout"),
		skipUntil: map[string]time.Time{}, emptyStreak: map[string]int{}, resSecs: resSecs}
}

func (e *Engine) Horizons() []time.Duration { return e.horizons }

// target is one (event, kind, horizon) waiting for a price in the current
// pass. Package-level because Engine.markStatus consumes it.
type target struct {
	due       time.Time
	event     string
	kind      string
	horizon   time.Duration
	basePrice float64
	baseMs    int64
}

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
	now := time.Now().UTC()
	// tokens still in backoff are excluded at the SQL level — if they filled
	// the LIMIT slots, the engine would skip them all and fresh pending rows
	// could starve behind a no_kline_data backlog.
	var backoffTokens []string
	for t, until := range e.skipUntil {
		if now.Before(until) {
			backoffTokens = append(backoffTokens, t)
		}
	}
	due, err := e.store.DueMarkouts(e.grace, now, 5000, backoffTokens)
	if err != nil {
		return err
	}
	if len(due) == 0 {
		return nil
	}

	// kline requests per tick — a batch of stale events must not spike the API.
	const maxTokensPerTick = 25

	byToken := map[string][]target{}
	tokenOrder := []string{} // keeps the SQL fresh-first order through the map
	earliest := map[string]time.Time{}
	for _, d := range due {
		if _, ok := byToken[d.Token]; !ok {
			tokenOrder = append(tokenOrder, d.Token)
		}
		byToken[d.Token] = append(byToken[d.Token], target{
			d.DueAt, d.EventID, d.Kind, d.Horizon, d.BasePrice, d.BaseMs,
		})
		if t, ok := earliest[d.Token]; !ok || d.DueAt.Before(t) {
			earliest[d.Token] = d.DueAt
		}
	}

	tokensDone := 0
	entryCache := map[string]entryPrice{} // event → entry (shared across its 6 horizon rows)
	for _, token := range tokenOrder {
		targets := byToken[token]
		if tokensDone >= maxTokensPerTick {
			break // rest retried next tick
		}
		if skip := e.skipUntil[token]; now.Before(skip) {
			continue // dead token, backoff not elapsed
		}
		if err := e.limiter.Take(ctx, 2); err != nil { // kline weight = 2
			return err
		}
		// kline window starts 2 resolutions before the earliest due time of
		// this token: the minimum horizon (1 res) + 1 closed candle for the
		// follower entry (lastCloseAtOrBefore needs open <= baseMs − res).
		lookback := 2 * time.Duration(e.resSecs) * time.Second
		candles, err := e.client.Kline(ctx, e.chain, token, e.res, earliest[token].Add(-lookback), now)
		if err != nil {
			if rl, ok := err.(*gmgn.RateLimitError); ok {
				// freeze the entire pipeline until the ban lifts; do NOT retry
				// this token this tick (every retry inside the window is +5s)
				e.limiter.MarkCooldown(rl.ResetAt)
				e.log.Warn("kline rate limited, cooling down", "token", token,
					"reset", rl.ResetAt.Format(time.RFC3339))
				e.markStatus(targets, storage.MarkoutStatusRateLimited)
				break // gate is shut; no point fetching more tokens this tick
			} else {
				e.log.Warn("kline fetch failed", "token", token, "err", err)
				e.markStatus(targets, storage.MarkoutStatusAPIError)
			}
			continue
		}
		tokensDone++
		if len(candles) == 0 {
			// empty kline: GMGN has no data for this token. That is a
			// MEASUREMENT failure (no_kline_data), NOT proof the token is
			// dead — the row stays retryable with an escalating backoff
			// instead of becoming a terminal -100% market loss.
			e.emptyStreak[token]++
			backoff := time.Duration(e.emptyStreak[token]) * 15 * time.Minute
			if backoff > time.Hour {
				backoff = time.Hour // ponytail: cap at 1h; revisit if tokens genuinely resurrect
			}
			e.skipUntil[token] = now.Add(backoff)
			e.markStatus(targets, storage.MarkoutStatusNoKlineData)
			continue
		}
		e.emptyStreak[token] = 0 // kline came back — reset the backoff ladder
		// candles with validity kept separate: an unparseable close is a
		// measurement failure (price_parse_error), NOT a dead token.
		scs := make([]sampledCandle, len(candles))
		for i, c := range candles {
			p, err := strconv.ParseFloat(c.Close, 64)
			scs[i] = sampledCandle{open: c.Time / 1000, close: p, valid: err == nil && p > 0}
		}
		for _, t := range targets {
			// follower rows need their entry price: the close of the last
			// candle ALREADY CLOSED at ReceivedAt (base_ms − res). The candle
			// in progress at base_ms is off-limits — its close contains up to
			// res of future information (P0.5 fix).
			if t.kind == storage.MarkoutFollower && t.basePrice == 0 {
				ep, ok := entryCache[t.event]
				if !ok {
					c, found := lastCloseAtOrBefore(scs, t.baseMs-e.resSecs)
					if !found {
						// entry candle out of the fetched window — retryable: a
						// later pass fetches a longer window.
						e.markStatus([]target{t}, storage.MarkoutStatusLookbackMiss)
						continue
					}
					if !c.valid {
						e.markStatus([]target{t}, storage.MarkoutStatusParseError)
						continue
					}
					ep = entryPrice{price: c.close, observedAt: time.Unix(c.open+e.resSecs, 0).UTC()}
					entryCache[t.event] = ep
				}
				if err := e.store.SetEntryPrice(t.event, t.kind, t.horizon, ep.price, ep.observedAt); err != nil {
					e.log.Warn("set entry price failed", "event", t.event, "err", err)
				}
				t.basePrice = ep.price
			}
			if t.basePrice <= 0 {
				e.markStatus([]target{t}, storage.MarkoutStatusLookbackMiss)
				continue // entry unknown; horizon return would be meaningless
			}
			c, found := firstCandleClosingAtOrAfter(scs, t.due.Unix(), e.resSecs)
			if !found {
				// no candle CLOSES at or after the due time (the stream's last
				// close is before due): the token stopped trading before the
				// horizon.
				e.markStatus([]target{t}, storage.MarkoutStatusNoCandle)
				continue
			}
			if !c.valid {
				// candle exists but its close is garbage — malformed API
				// response, NOT a dead token.
				e.markStatus([]target{t}, storage.MarkoutStatusParseError)
				continue
			}
			// the horizon price is the close of the first candle whose CLOSE
			// time (open+res) is >= due; observation lag is therefore strictly
			// in [0, res]. If even that close lands after due+res (open > due)
			// there was no executable price at the fixed horizon.
			closeTime := c.open + e.resSecs
			if closeTime > t.due.Unix()+e.resSecs {
				e.markStatus([]target{t}, storage.MarkoutStatusStaleOutcome)
				continue
			}
			if err := e.store.FillMarkout(t.event, t.kind, t.horizon, c.close, closeTime); err != nil {
				e.log.Warn("fill markout failed", "event", t.event, "horizon", t.horizon, "err", err)
			}
		}
	}
	return nil
}

// markStatus labels rows with why they could not be filled. Sticky market
// statuses (no_candle, token_inactive) survive later transient failures;
// transient ones get overwritten by whatever the next pass observes.
func (e *Engine) markStatus(targets []target, status string) {
	for _, t := range targets {
		if err := e.store.SetMarkoutStatus(t.event, t.kind, t.horizon, status); err != nil {
			e.log.Warn("set markout status failed", "event", t.event, "status", status, "err", err)
		}
	}
}

// entryPrice is a follower entry sampled from klines, with the instant it
// actually represents.
type entryPrice struct {
	price      float64
	observedAt time.Time
}

// sampledCandle is one kline candle with its close parsed; valid=false
// means the close was unparseable or non-positive — a measurement failure
// that must NOT be mistaken for a dead token.
type sampledCandle struct {
	open  int64 // candle open, unix seconds
	close float64
	valid bool
}

// firstCandleClosingAtOrAfter returns the first candle whose CLOSE time
// (open+res) is at or after ts — i.e. the first candle with open >= ts-res.
// The observation instant of the returned price is open+res, so the lag vs
// ts is bounded by [0, res], not [0, 2×res] as an open-based search would
// allow. found=false means the stream ended before ts; the returned candle
// may still be invalid (unparseable close). cs must be ascending by open.
func firstCandleClosingAtOrAfter(cs []sampledCandle, ts, resSecs int64) (sampledCandle, bool) {
	i := sort.Search(len(cs), func(i int) bool { return cs[i].open >= ts-resSecs })
	if i == len(cs) {
		return sampledCandle{}, false
	}
	return cs[i], true
}

// lastCloseAtOrBefore returns the last candle opening at or before ts — used
// for follower entries: the newest candle whose close (open+res) is already
// known at ts, i.e. NO future info. found=false when the stream starts after
// ts; the returned candle may still be invalid.
func lastCloseAtOrBefore(cs []sampledCandle, ts int64) (sampledCandle, bool) {
	i := sort.Search(len(cs), func(i int) bool { return cs[i].open > ts })
	if i == 0 {
		return sampledCandle{}, false
	}
	return cs[i-1], true
}
