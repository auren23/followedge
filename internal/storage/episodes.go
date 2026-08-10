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
}

// RebuildEpisodes reconstructs ALL wallets' position episodes since `since`
// and materializes them into position_episodes. Deterministic and idempotent
// (table wiped and rebuilt). Same-second trades are ordered by
// received_at then event_id — approximate without a chain tx index.
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

// reconstructEpisodes builds episodes from an ordered event stream
// (wallet, token, side, token_amount, amount_usd, buy_cost_usd, trade_time,
// received_at, event_id — ascending by time). Shared by the global rebuild
// and the per-wallet on-demand reconstruction.
//
// Quantity arithmetic uses a RELATIVE epsilon (1e-9 of the peak visible
// quantity): large meme-token counts with float64 residue must not turn a
// perfect full close into a data-gap partial.
func reconstructEpisodes(rows *sql.Rows) ([]Episode, error) {
	var eps []Episode
	var cur *Episode
	var curWallet, curToken string
	var qty, peakQty float64 // visible token quantity + its peak (epsilon scale)

	epsOf := func() float64 {
		if peakQty <= 0 {
			return 1e-9
		}
		return math.Max(1e-9, peakQty*1e-9)
	}

	flush := func() {
		if cur != nil {
			eps = append(eps, *cur)
			cur = nil
		}
		qty, peakQty = 0, 0
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
			curWallet, curToken = wallet, token
			cur = &Episode{Wallet: wallet, Token: token, OpenedAt: int64(ts), Status: EpisodeOpen}
			cur.DataGap = side == "sell" // window opened on a sell: opening buy unseen
		}
		if side == "buy" {
			if qty <= epsOf() {
				// opening buy (or re-opening after a full close)
				cur.OpenedAt = int64(ts)
				cur.InitialBuyUSD = amountUSD
				cur.DataGap = false
				cur.FirstSellAt = 0
			} else {
				cur.Adds++
				cur.AddBuyUSD += amountUSD
			}
			qty += tokenAmount
			cur.CapitalIn += amountUSD
		} else { // sell
			if qty <= epsOf() {
				cur.DataGap = true // selling with no visible position
			}
			if cur.FirstSellAt == 0 {
				cur.FirstSellAt = int64(ts)
			}
			cur.SellLegs++
			cur.Reduces++
			qty -= tokenAmount
			cur.CapitalOut += amountUSD
			if buyCost.Valid && buyCost.Float64 > 0 {
				cur.RealizedPnL += amountUSD - buyCost.Float64
			}
			if qty > epsOf() {
				cur.PartialExitLegs++ // position remains: a REAL partial exit
			}
			if math.Abs(qty) <= epsOf() {
				qty = 0
				cur.Status = EpisodeClosed
				cur.ClosedAt = int64(ts)
				cur.HoldDurationS = cur.ClosedAt - cur.OpenedAt
				flush()
			} else if qty < -epsOf() {
				cur.Status = EpisodePartial // sold more than visible position
				cur.DataGap = true
				qty = 0
				cur.ClosedAt = int64(ts)
				cur.HoldDurationS = cur.ClosedAt - cur.OpenedAt
				flush()
			}
		}
		if math.Abs(qty) > peakQty {
			peakQty = math.Abs(qty)
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
