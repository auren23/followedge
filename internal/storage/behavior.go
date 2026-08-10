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
func (s *Store) EntryRows(wallet string, since time.Time) ([]EntryRow, error) {
	rows, err := s.db.Query(`
		SELECT token_address, amount_usd, trade_time, received_at
		FROM trade_events
		WHERE wallet = ? AND side = 'buy' AND trade_time >= ?
		ORDER BY trade_time`, wallet, since.Unix())
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

// EpisodesFor lists a wallet's reconstructed position episodes, newest first.
func (s *Store) EpisodesFor(wallet string) ([]Episode, error) {
	rows, err := s.db.Query(`
		SELECT wallet, token, opened_at, closed_at, adds, reduces,
		       capital_in, capital_out, realized_pnl, hold_duration_s, status
		FROM position_episodes
		WHERE wallet = ?
		ORDER BY opened_at DESC`, wallet)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Episode
	for rows.Next() {
		var e Episode
		var closed, hold sql.NullInt64
		if err := rows.Scan(&e.Wallet, &e.Token, &e.OpenedAt, &closed, &e.Adds,
			&e.Reduces, &e.CapitalIn, &e.CapitalOut, &e.RealizedPnL, &hold, &e.Status); err != nil {
			return nil, err
		}
		e.ClosedAt = closed.Int64
		e.HoldDurationS = hold.Int64
		out = append(out, e)
	}
	return out, rows.Err()
}

// FirstSellDelays returns, per position episode, the seconds between the
// episode's opening buy and its FIRST sell leg (time-to-first-sell).
// Episodes that never sold are skipped. Reconstructed in Go from the same
// ordered event stream as RebuildEpisodes.
func (s *Store) FirstSellDelays(wallet string, since time.Time) ([]int64, error) {
	rows, err := s.db.Query(`
		SELECT token_address, side, token_amount, trade_time
		FROM trade_events
		WHERE wallet = ? AND trade_time >= ?
		ORDER BY token_address, trade_time`, wallet, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	var curToken string
	var openTime int64
	qty := 0.0
	firstSold := false
	for rows.Next() {
		var token, side string
		var amt, ts float64
		if err := rows.Scan(&token, &side, &amt, &ts); err != nil {
			return nil, err
		}
		if token != curToken {
			curToken, openTime, qty, firstSold = token, int64(ts), 0, false
		}
		if side == "buy" {
			if qty <= 0 {
				openTime = int64(ts) // (re)opening buy
				firstSold = false
			}
			qty += amt
		} else if qty > 0 {
			if !firstSold {
				out = append(out, int64(ts)-openTime) // first sell leg of this episode
				firstSold = true
			}
			qty -= amt
			if qty <= 0 {
				firstSold = false // episode closed; next buy opens a new one
			}
		}
	}
	return out, rows.Err()
}

// ClusterStateAt returns the most recent cluster snapshot for a token whose
// window state is knowable AT or BEFORE `at` — point-in-time: a snapshot
// taken after the entry must not describe the entry's context.
func (s *Store) ClusterStateAt(token string, at int64, window time.Duration) (ClusterSampleRow, bool, error) {
	var r ClusterSampleRow
	var wms int64
	err := s.db.QueryRow(`
		SELECT token_address, window_ms, ts,
		       smart_buy_wallets, smart_sell_wallets, kol_buy_wallets, kol_sell_wallets,
		       net_smart_flow_usd, event_count
		FROM cluster_samples
		WHERE token_address = ? AND window_ms = ? AND ts <= ?
		ORDER BY ts DESC LIMIT 1`,
		token, window.Milliseconds(), at).
		Scan(&r.TokenAddress, &wms, &r.TS,
			&r.SmartBuyWallets, &r.SmartSellWallets, &r.KOLBuyWallets, &r.KOLSellWallets,
			&r.NetFlowUSD, &r.EventCount)
	if err == sql.ErrNoRows {
		return ClusterSampleRow{}, false, nil
	}
	if err != nil {
		return ClusterSampleRow{}, false, err
	}
	r.Window = time.Duration(wms) * time.Millisecond
	return r, true, nil
}
