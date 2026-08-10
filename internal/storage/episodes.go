package storage

import (
	"database/sql"
	"fmt"
	"math"
	"time"
)

// Episode statuses.
const (
	EpisodeOpen    = "open"
	EpisodeClosed  = "closed"
	EpisodePartial = "partial" // sell legs exceeded visible position (data gap)
)

// Episode is one reconstructed position round-trip of a (wallet, token).
//
// Status semantics (v0.2.0.1): "partial" means the sell legs EXCEEDED the
// visible position — a DATA GAP (our window missed the opening buys), NOT
// an intentional partial-exit behavior. Real partial exits are counted
// separately in PartialExitLegs: sell legs that left a positive visible
// quantity behind.
type Episode struct {
	Wallet        string
	Token         string
	OpenedAt      int64
	ClosedAt      int64 // 0 while open
	Adds          int
	Reduces       int
	CapitalIn     float64
	CapitalOut    float64
	RealizedPnL   float64
	HoldDurationS int64
	Status        string

	// v0.2.0.1 behavior facts (reconstructed; not persisted in
	// position_episodes — behavior analysis never depends on the table).
	FirstSellAt     int64   // first sell leg time, 0 = never sold
	InitialBuyUSD   float64 // opening buy notional (0 if opening buy unseen)
	AddBuyUSD       float64 // total notional of add buys
	SellLegs        int     // total sell legs
	PartialExitLegs int     // sell legs that left visible qty > 0 (real partial exit)
	DataGap         bool    // opening buy (or part of the position) unseen

	// OriginQuality tracks what we KNOW about this episode's opening:
	//   Censored     — the collector may have missed an earlier position;
	//                  the first visible buy could be an add.
	//   VisibleZero  — our visible ledger reached zero, but that does NOT
	//                  prove the wallet's real balance is zero (a hidden
	//                  pre-dataset position may remain). Inferred, research
	//                  only.
	//   ConfirmedZero— independent evidence (future: on-chain balance = 0)
	//                  proves the position reached zero. NO current source
	//                  provides this, so it is never assigned yet.
	// Mechanism analysis must only consume initial-entry features of
	// OriginConfirmedZero episodes.
	OriginQuality OriginQuality
}

// OriginQuality rates the evidence behind an episode's opening buy.
type OriginQuality int

const (
	OriginCensored OriginQuality = iota
	OriginVisibleZero
	OriginConfirmedZero
)

// RebuildEpisodes reconstructs ALL wallets' position episodes since `since`
// and materializes them into position_episodes.
//
// LEGACY/DEBUG CACHE: left-truncated at `since` and only used by the
// `analyze episodes` debug command. Mechanism analysis MUST use the
// on-demand ReconstructEpisodesFor (full history + cohort filter) — never
// this table. Deterministic and idempotent (table wiped and rebuilt).
// Same-second trades are ordered by received_at then event_id (approximate
// without a chain tx index).
func (s *Store) RebuildEpisodes(since time.Time) (int, error) {
	rows, err := s.db.Query(`
		SELECT wallet, token_address, side, token_amount, amount_usd,
		       buy_cost_usd, trade_time, received_at, event_id
		FROM trade_events
		WHERE trade_time >= ?
		ORDER BY wallet, token_address, trade_time, received_at, event_id`, since.Unix())
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	eps, err := reconstructEpisodes(rows)
	if err != nil {
		return 0, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM position_episodes"); err != nil {
		return 0, err
	}
	for _, e := range eps {
		var closed any
		if e.ClosedAt > 0 {
			closed = e.ClosedAt
		}
		var hold any
		if e.HoldDurationS > 0 {
			hold = e.HoldDurationS
		}
		if _, err := tx.Exec(`
			INSERT INTO position_episodes
			(wallet, token, opened_at, closed_at, adds, reduces,
			 capital_in, capital_out, realized_pnl, hold_duration_s, status)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			e.Wallet, e.Token, e.OpenedAt, closed, e.Adds, e.Reduces,
			e.CapitalIn, e.CapitalOut, e.RealizedPnL, hold, e.Status); err != nil {
			return 0, fmt.Errorf("insert episode: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(eps), nil
}

// ReconstructEpisodesFor rebuilds ONE wallet's episodes on demand, straight
// from trade_events — the behavior card must never depend on a stale or
// never-materialized position_episodes table.
//
// Left-truncation guard: reconstruction runs over the wallet's FULL history
// and only the final episodes (opened_at >= since) are returned — an episode
// opened before the analysis window keeps its real InitialBuyUSD / OpenedAt
// instead of mistaking the first visible add for the opening buy.
func (s *Store) ReconstructEpisodesFor(wallet string, since time.Time) ([]Episode, error) {
	rows, err := s.db.Query(`
		SELECT wallet, token_address, side, token_amount, amount_usd,
		       buy_cost_usd, trade_time, received_at, event_id
		FROM trade_events
		WHERE wallet = ?
		ORDER BY token_address, trade_time, received_at, event_id`, wallet)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	eps, err := reconstructEpisodes(rows)
	if err != nil {
		return nil, err
	}
	out := eps[:0]
	for _, e := range eps {
		if e.OpenedAt >= since.Unix() {
			out = append(out, e)
		}
	}
	return out, nil
}

// PositionState is what a sell leg left behind.
type PositionState int

const (
	Remaining PositionState = iota // qty > eps: position still open
	Closed                         // |qty| <= eps: full close, snapped to zero
	Oversold                       // qty < -eps: sold more than visible (data gap)
)

// positionBook tracks a (wallet, token) visible quantity with a RELATIVE
// epsilon (1e-9 of peak): float64 residue on large meme-token counts must
// not flip a full close into a data gap. Single source of truth for both
// episode reconstruction and entry classification — the two must never
// disagree on whether a position is open.
type positionBook struct {
	qty, peak float64
}

func (b *positionBook) eps() float64 {
	if b.peak <= 0 {
		return 1e-9
	}
	return math.Max(1e-9, b.peak*1e-9)
}

func (b *positionBook) add(amount float64) {
	b.qty += amount
	if b.qty > b.peak {
		b.peak = b.qty
	}
}

// sell subtracts and reports the resulting state. A full close RESETS the
// peak — otherwise the next episode would inherit the previous episode's
// epsilon and small opening buys would look empty (misclassified as
// initial when they are adds).
func (b *positionBook) sell(amount float64) PositionState {
	b.qty -= amount
	switch {
	case math.Abs(b.qty) <= b.eps():
		b.qty, b.peak = 0, 0
		return Closed
	case b.qty < -b.eps():
		return Oversold
	}
	if b.qty > b.peak {
		b.peak = b.qty
	}
	return Remaining
}

func (b *positionBook) isEmpty() bool { return b.qty <= b.eps() }
func (b *positionBook) reset()        { b.qty, b.peak = 0, 0 }

func reconstructEpisodes(rows *sql.Rows) ([]Episode, error) {
	var eps []Episode
	var cur *Episode
	var curWallet, curToken string
	book := &positionBook{}
	origin := OriginCensored // per token group: quality of the NEXT episode's opening

	flush := func() {
		if cur != nil {
			if cur.Status == EpisodeClosed {
				// Our ledger reached zero — inferred, NOT confirmed: a hidden
				// pre-dataset position could still be open.
				origin = OriginVisibleZero
			}
			eps = append(eps, *cur)
			cur = nil
		}
		book.reset()
	}

	for rows.Next() {
		var wallet, token, side string
		var tokenAmount, amountUSD, ts, received float64
		var buyCost sql.NullFloat64
		if err := rows.Scan(&wallet, &token, &side, &tokenAmount, &amountUSD,
			&buyCost, &ts, &received, new(string)); err != nil {
			return nil, err
		}
		if cur == nil || wallet != curWallet || token != curToken {
			flush()
			if wallet != curWallet || token != curToken {
				origin = OriginCensored // new (wallet, token) group: censored again
			}
			curWallet, curToken = wallet, token
			cur = &Episode{Wallet: wallet, Token: token, OpenedAt: int64(ts), Status: EpisodeOpen}
			// A visible full close only upgrades to VisibleZero — it cannot
			// prove the wallet's real balance is zero (hidden pre-dataset
			// position), so OriginConfirmedZero is never reached with the
			// current data sources.
			cur.OriginQuality = origin
			if side == "sell" {
				cur.OriginQuality = OriginCensored // opening buy unseen
			}
			cur.DataGap = side == "sell" // window opened on a sell: opening buy unseen
		}
		if side == "buy" {
			if book.isEmpty() {
				// opening buy (or re-opening after a full close)
				cur.OpenedAt = int64(ts)
				cur.InitialBuyUSD = amountUSD
				cur.DataGap = false
				cur.FirstSellAt = 0
			} else {
				cur.Adds++
				cur.AddBuyUSD += amountUSD
			}
			book.add(tokenAmount)
			cur.CapitalIn += amountUSD
		} else { // sell
			if book.isEmpty() {
				cur.DataGap = true // selling with no visible position
			}
			if cur.FirstSellAt == 0 {
				cur.FirstSellAt = int64(ts)
			}
			cur.SellLegs++
			cur.Reduces++
			cur.CapitalOut += amountUSD
			if buyCost.Valid && buyCost.Float64 > 0 {
				cur.RealizedPnL += amountUSD - buyCost.Float64
			}
			switch book.sell(tokenAmount) {
			case Remaining:
				cur.PartialExitLegs++ // position remains: a REAL partial exit
			case Closed:
				cur.Status = EpisodeClosed
				cur.ClosedAt = int64(ts)
				cur.HoldDurationS = cur.ClosedAt - cur.OpenedAt
				flush()
			case Oversold:
				cur.Status = EpisodePartial // sold more than visible position
				cur.DataGap = true
				cur.ClosedAt = int64(ts)
				cur.HoldDurationS = cur.ClosedAt - cur.OpenedAt
				// an oversold leg proves our trajectory has a gap — the
				// next episode's origin confidence drops back to Censored
				origin = OriginCensored
				flush()
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// the last episode per group stays open
	flush()
	return eps, nil
}

// EpisodeCounts aggregates episodes for analysis.
type EpisodeCounts struct {
	Total, Closed, Open, Partial int
	AvgHoldSecs                  int64
	TotalPnl                     float64
	Profitable                   int
	AvgPnl                       float64
}

func (s *Store) EpisodeStats() (EpisodeCounts, error) {
	var c EpisodeCounts
	rows, err := s.db.Query(`
		SELECT status, COUNT(*), COALESCE(AVG(hold_duration_s),0),
		       COALESCE(SUM(realized_pnl),0),
		       SUM(CASE WHEN realized_pnl > 0 THEN 1 ELSE 0 END)
		FROM position_episodes GROUP BY status`)
	if err != nil {
		return c, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var n int
		var avgHold, sumPnl, wins float64
		if err := rows.Scan(&status, &n, &avgHold, &sumPnl, &wins); err != nil {
			return c, err
		}
		c.Total += n
		switch status {
		case EpisodeClosed:
			c.Closed += n
			c.AvgHoldSecs = int64(avgHold)
			c.AvgPnl = sumPnl / float64(n)
		case EpisodeOpen:
			c.Open += n
		case EpisodePartial:
			c.Partial += n
		}
		if status == EpisodeClosed {
			c.TotalPnl += sumPnl
			c.Profitable += int(wins)
		}
	}
	return c, rows.Err()
}
