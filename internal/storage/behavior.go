package storage

import (
	"database/sql"
	"time"
)

// EntryRow is one buy event of a wallet — the entry-time context for
// behavior reconstruction (v0.2.0).
type EntryRow struct {
	Token      string
	AmountUSD  float64
	TradeTime  int64
	ReceivedAt int64
}

// EntryRows lists a wallet's BUY events since `since` (entry context).
// Same-second trades are ordered by received_at, event_id (approximate
// without a chain tx index).
func (s *Store) EntryRows(wallet string, since time.Time) ([]EntryRow, error) {
	rows, err := s.db.Query(`
		SELECT token_address, amount_usd, trade_time, received_at
		FROM trade_events
		WHERE wallet = ? AND side = 'buy' AND trade_time >= ?
		ORDER BY trade_time, received_at, event_id`, wallet, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EntryRow
	for rows.Next() {
		var r EntryRow
		if err := rows.Scan(&r.Token, &r.AmountUSD, &r.TradeTime, &r.ReceivedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// EntryObservation is the chase feature of ONE entry, knowable at entry
// time (follower entry price sampled) — deliberately NOT conditioned on the
// horizon markout being filled, so median chase has no survivor bias.
type EntryObservation struct {
	EventID   string
	TradeTime int64
	ChasePct  float64 // (entry price / leader price - 1) * 100
}

// EntryObservations returns one chase observation per buy entry of the
// wallet whose follower entry price was sampled (base_price NOT NULL).
// Requires the entry observation, never the horizon outcome.
func (s *Store) EntryObservations(wallet string, since time.Time, horizon time.Duration) ([]EntryObservation, error) {
	rows, err := s.db.Query(`
		SELECT e.event_id, e.trade_time, m.base_price, e.price_usd
		FROM markouts m
		JOIN trade_events e ON e.event_id = m.event_id
		WHERE m.kind = 'follower' AND m.horizon_ms = ?
		  AND m.base_price IS NOT NULL
		  AND e.side = 'buy' AND e.wallet = ? AND e.trade_time >= ?
		ORDER BY e.trade_time`, horizon.Milliseconds(), wallet, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EntryObservation
	for rows.Next() {
		var e EventRow
		var base, leader sql.NullFloat64
		if err := rows.Scan(&e.ID, &e.TradeTime, &base, &leader); err != nil {
			return nil, err
		}
		if base.Valid && leader.Valid && base.Float64 > 0 && leader.Float64 > 0 {
			out = append(out, EntryObservation{
				EventID:   e.ID,
				TradeTime: e.TradeTime,
				ChasePct:  (base.Float64/leader.Float64 - 1) * 100,
			})
		}
	}
	return out, rows.Err()
}

// PriorFlowAt counts distinct smart-money and KOL BUYERS of a token whose
// trades happened in [entryTime-window, entryTime) — the market context the
// actor saw BEFORE its entry, recomputed historically from trade_events.
//
// The upper bound is STRICTLY < entryTime: without a chain tx index the
// same-second ordering is unknowable, so same-second trades are conserva-
// tively excluded (they could be the actor's own buy).
func (s *Store) PriorFlowAt(token string, entryTime int64, window time.Duration) (smart, kol int, err error) {
	err = s.db.QueryRow(`
		SELECT COUNT(DISTINCT CASE WHEN wallet_type = 'smart_money' AND side = 'buy' THEN wallet END),
		       COUNT(DISTINCT CASE WHEN wallet_type = 'kol' AND side = 'buy' THEN wallet END)
		FROM trade_events
		WHERE token_address = ? AND trade_time >= ? AND trade_time < ?`,
		token, entryTime-window.Milliseconds()/1000, entryTime).
		Scan(&smart, &kol)
	if err == sql.ErrNoRows {
		return 0, 0, nil
	}
	return smart, kol, err
}
