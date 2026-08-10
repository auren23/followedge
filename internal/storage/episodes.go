package storage

import (
	"database/sql"
	"fmt"
	"time"
)

// Episode statuses.
const (
	EpisodeOpen    = "open"
	EpisodeClosed  = "closed"
	EpisodePartial = "partial" // sell legs exceeded visible position (data gap)
)

// Episode is one reconstructed position round-trip of a (wallet, token).
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
}

// RebuildEpisodes reconstructs position episodes from trade_events since
// `since` (per wallet+token, ordered by trade_time). Deterministic and
// idempotent: the table is wiped and rebuilt.
//
// Limits: the feed shows only recent trades, so an episode whose opening
// buys predate our window looks "partial" when sells exceed visible
// position; that is recorded explicitly rather than guessed.
func (s *Store) RebuildEpisodes(since time.Time) (int, error) {
	rows, err := s.db.Query(`
		SELECT wallet, token_address, side, token_amount, amount_usd,
		       buy_cost_usd, trade_time
		FROM trade_events
		WHERE trade_time >= ?
		ORDER BY wallet, token_address, trade_time`, since.Unix())
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var eps []Episode
	var cur *Episode
	var curWallet, curToken string
	var qty float64 // visible token quantity

	flush := func() {
		if cur != nil {
			eps = append(eps, *cur)
			cur = nil
		}
		qty = 0
	}

	for rows.Next() {
		var wallet, token, side string
		var tokenAmount, amountUSD, ts float64
		var buyCost sql.NullFloat64
		if err := rows.Scan(&wallet, &token, &side, &tokenAmount, &amountUSD, &buyCost, &ts); err != nil {
			return 0, err
		}
		if cur == nil || wallet != curWallet || token != curToken {
			flush()
			curWallet, curToken = wallet, token
			cur = &Episode{Wallet: wallet, Token: token, OpenedAt: int64(ts), Status: EpisodeOpen}
		}
		if side == "buy" {
			qty += tokenAmount
			cur.CapitalIn += amountUSD
			if qty-tokenAmount <= 0 { // first buy of the episode
				cur.OpenedAt = int64(ts)
			} else {
				cur.Adds++
			}
		} else { // sell
			qty -= tokenAmount
			cur.Reduces++
			cur.CapitalOut += amountUSD
			if buyCost.Valid && buyCost.Float64 > 0 {
				cur.RealizedPnL += amountUSD - buyCost.Float64
			}
			if qty <= 0 {
				if qty < 0 {
					cur.Status = EpisodePartial // sold more than visible position
				} else {
					cur.Status = EpisodeClosed
				}
				qty = 0
				cur.ClosedAt = int64(ts)
				cur.HoldDurationS = cur.ClosedAt - cur.OpenedAt
				flush()
			}
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	// the last episode per group stays open
	flush()

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
