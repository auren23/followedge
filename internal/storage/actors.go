package storage

import (
	"database/sql"
	"time"

	"github.com/auren23/followedge/internal/domain"
)

// UpsertActor registers a wallet in the actor universe (or refreshes its
// last-seen). Called once per newly created event.
func (s *Store) UpsertActor(e domain.TradeEvent) error {
	_, err := s.db.Exec(`
		INSERT INTO actors (address, actor_type, chain, first_seen_at, last_seen_at, trade_count)
		VALUES (?,?,?,?,?,1)
		ON CONFLICT(address) DO UPDATE SET
			last_seen_at = MAX(last_seen_at, excluded.last_seen_at),
			first_seen_at = MIN(first_seen_at, excluded.first_seen_at),
			trade_count = trade_count + 1`,
		e.Wallet, string(e.WalletType), e.Chain,
		e.TradeTime.Unix(), e.TradeTime.Unix())
	return err
}

// ActorGroup is one (wallet, token, day) bucket from the raw query.
type ActorGroup struct {
	Wallet      string
	WalletType  string
	Token       string
	Day         string  // YYYY-MM-DD
	RealizedPnL float64 // sells only: amount_usd - buy_cost_usd
	Trades      int
	Buys        int
	Sells       int
	TotalUSD    float64
	FirstTs     int64
	LastTs      int64
}

// ActorGroups returns per-(wallet, token, day) aggregates since `since`
// (trade_time). One query feeds realized PnL, concentration, consistency and
// drawdown — the ranking layer buckets it in Go.
func (s *Store) ActorGroups(since time.Time) ([]ActorGroup, error) {
	rows, err := s.db.Query(`
		SELECT wallet, wallet_type, token_address,
		       date(trade_time, 'unixepoch'),
		       SUM(CASE WHEN side = 'sell' AND buy_cost_usd > 0 THEN amount_usd - buy_cost_usd ELSE 0 END),
		       COUNT(*),
		       SUM(CASE WHEN side = 'buy' THEN 1 ELSE 0 END),
		       SUM(CASE WHEN side = 'sell' THEN 1 ELSE 0 END),
		       SUM(amount_usd),
		       MIN(trade_time), MAX(trade_time)
		FROM trade_events
		WHERE trade_time >= ?
		GROUP BY wallet, token_address, date(trade_time, 'unixepoch')`,
		since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActorGroup
	for rows.Next() {
		var g ActorGroup
		var pnl sql.NullFloat64
		if err := rows.Scan(&g.Wallet, &g.WalletType, &g.Token, &g.Day,
			&pnl, &g.Trades, &g.Buys, &g.Sells, &g.TotalUSD, &g.FirstTs, &g.LastTs); err != nil {
			return nil, err
		}
		g.RealizedPnL = pnl.Float64
		out = append(out, g)
	}
	return out, rows.Err()
}

// ActorCount returns the size of the actor universe.
func (s *Store) ActorCount() (int64, error) {
	var n int64
	err := s.db.QueryRow("SELECT COUNT(*) FROM actors").Scan(&n)
	return n, err
}
