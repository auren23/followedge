package storage

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/auren23/followedge/internal/domain"
)

// Markout kinds — the two questions markouts answer. Analysis MUST filter by
// kind; leader EV and follower EV are different measurements.
const (
	MarkoutLeader   = "leader"   // base = TradeTime,  base_price = leader's price_usd
	MarkoutFollower = "follower" // base = ReceivedAt, base_price = price at ReceivedAt (sampled later)
)

// CreateMarkouts reserves one row per horizon for an event under one kind.
// basePrice is the entry price: leader rows have it immediately (the event's
// price_usd), follower rows get it sampled from klines later (pass nil).
func (s *Store) CreateMarkouts(e domain.TradeEvent, kind string, basePrice *float64, horizons []time.Duration, createdAt time.Time) error {
	baseMs := e.TradeTime.Unix()
	if kind == MarkoutFollower {
		baseMs = e.ReceivedAt.Unix()
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, h := range horizons {
		var bp any
		if basePrice != nil {
			bp = *basePrice
		}
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO markouts (event_id, kind, horizon_ms, base_ms, base_price, created_at)
			 VALUES (?,?,?,?,?,?)`,
			e.ID, kind, h.Milliseconds(), baseMs, bp, createdAt.Unix()); err != nil {
			return fmt.Errorf("create %s markout %v: %w", kind, h, err)
		}
	}
	return tx.Commit()
}

// DueMarkout is one (event, kind, horizon) waiting for price sampling.
type DueMarkout struct {
	EventID   string
	Kind      string
	Token     string
	Horizon   time.Duration
	BasePrice float64 // 0 until entry price sampled (follower)
	BaseMs    int64   // sampling base, unix seconds
	DueAt     time.Time
}

// DueMarkouts lists rows whose sampling time (plus grace) has passed:
//   - follower rows with base_price NULL need their ReceivedAt entry price
//   - any row with base_price set and base_ms+horizon <= cutoff needs its
//     horizon price
//
// TERMINAL market statuses (no_candle, token_inactive, stale_outcome) are
// excluded: their observed_price stays NULL forever, so re-listing them
// every tick would burn kline quota on rows that can never fill.
func (s *Store) DueMarkouts(grace time.Duration, now time.Time, limit int) ([]DueMarkout, error) {
	cutoff := now.Add(-grace).Unix()
	// follower rows first (the measurement that matters), transient failures
	// next (api_error/rate_limited/lookback_miss can still recover, and a
	// fresh kline window is most likely to fix them), then newest first —
	// stale leader rows from dead tokens must not starve fresh data.
	rows, err := s.db.Query(`
		SELECT m.event_id, m.kind, m.horizon_ms, m.base_price, m.base_ms,
		       e.token_address
		FROM markouts m
		JOIN trade_events e ON e.event_id = m.event_id
		WHERE m.observed_price IS NULL
		  AND m.base_ms + m.horizon_ms/1000 <= ?
		  AND m.status IN ('pending','lookback_miss','api_error','rate_limited','price_parse_error','no_kline_data')
		ORDER BY (m.kind = 'follower') DESC,
		         (m.status IN ('lookback_miss','api_error','rate_limited','no_kline_data')) DESC,
		         m.base_ms + m.horizon_ms/1000 DESC
		LIMIT ?`, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DueMarkout
	for rows.Next() {
		var d DueMarkout
		var hms sql.NullInt64
		var bp sql.NullFloat64
		if err := rows.Scan(&d.EventID, &d.Kind, &hms, &bp, &d.BaseMs, &d.Token); err != nil {
			return nil, err
		}
		d.Horizon = time.Duration(hms.Int64) * time.Millisecond
		d.BasePrice = bp.Float64
		d.DueAt = time.Unix(d.BaseMs, 0).Add(d.Horizon)
		out = append(out, d)
	}
	return out, rows.Err()
}

// SetEntryPrice fills a follower row's base_price with the price a follower
// could have traded at reception: the close of the last candle ALREADY
// CLOSED at ReceivedAt (never look-ahead — see migration 004). observedAt is
// the instant that price actually represents (candle open + resolution).
func (s *Store) SetEntryPrice(eventID, kind string, horizon time.Duration, entryPrice float64, observedAt time.Time) error {
	_, err := s.db.Exec(`
		UPDATE markouts SET base_price = ?, entry_observed_at = ?
		WHERE event_id = ? AND kind = ? AND horizon_ms = ? AND base_price IS NULL`,
		entryPrice, observedAt.Unix(), eventID, kind, horizon.Milliseconds())
	return err
}

// Markout status — why a row is (not) filled. Every non-filled status is a
// coverage loss that EV tables must surface; unpriced tokens are usually the
// worst performers, so silently excluding them biases EV upward.
const (
	MarkoutStatusPending       = "pending"           // not yet due / not yet classified
	MarkoutStatusFilled        = "filled"            // horizon price sampled
	MarkoutStatusNoCandle      = "no_candle"         // candle stream ended before horizon (token stopped trading)
	MarkoutStatusTokenInactive = "token_inactive"    // confirmed dead token (currently reserved: needs on-chain evidence)
	MarkoutStatusStaleOutcome  = "stale_outcome"     // stream continued but first candle close landed past due+res
	MarkoutStatusNoKlineData   = "no_kline_data"     // GMGN returned an empty kline — measurement, NOT proof the token is dead
	MarkoutStatusAPIError      = "api_error"         // kline request failed (non-429)
	MarkoutStatusRateLimited   = "rate_limited"      // 429; gate closed
	MarkoutStatusLookbackMiss  = "lookback_miss"     // entry candle out of fetched range
	MarkoutStatusParseError    = "price_parse_error" // kline close unparseable
)

// FillMarkout writes the sampled horizon price, return and the observation
// instant (candle close time) for one row.
func (s *Store) FillMarkout(eventID, kind string, horizon time.Duration, observed float64, observedAt int64) error {
	_, err := s.db.Exec(`
		UPDATE markouts SET observed_price = ?,
			return_pct = CASE WHEN base_price > 0 THEN ( ? / base_price - 1 ) * 100 ELSE NULL END,
			outcome_observed_at = ?,
			status = ?
		WHERE event_id = ? AND kind = ? AND horizon_ms = ? AND observed_price IS NULL`,
		observed, observed, observedAt, MarkoutStatusFilled, eventID, kind, horizon.Milliseconds())
	return err
}

// SetMarkoutStatus records why a row could not be filled.
//
// Statuses split into two dimensions (see MarkoutStatus* consts):
//   - market outcome (no_candle, token_inactive, stale_outcome) is STICKY —
//     a dead token stays dead, and only a later successful fill overwrites it;
//   - measurement failure (api_error, rate_limited, lookback_miss,
//     price_parse_error, no_kline_data) is RETRYABLE — a later pass may
//     recover (a fresh kline window, the 429 gate reopening), so any of
//     these can be overwritten by any other status, including another
//     failure reason (e.g. price_parse_error → no_candle).
func (s *Store) SetMarkoutStatus(eventID, kind string, horizon time.Duration, status string) error {
	_, err := s.db.Exec(`
		UPDATE markouts SET status = ?
		WHERE event_id = ? AND kind = ? AND horizon_ms = ?
		  AND status IN ('pending','lookback_miss','api_error','rate_limited','price_parse_error','no_kline_data')`,
		status, eventID, kind, horizon.Milliseconds())
	return err
}

// MarkoutStat is one analyzed markout, with the chase number attached.
type MarkoutStat struct {
	EventID           string
	Wallet            string
	WalletType        string
	Side              string
	AmountUSD         float64
	TradeTime         int64
	ReceivedAt        int64
	Horizon           time.Duration
	ReturnPct         sql.NullFloat64 // follower/leader forward return at horizon
	BasePrice         float64         // entry price (leader: price_usd; follower: ReceivedAt price)
	LeaderPrice       float64         // leader's price_usd, for chase
	EntryObservedAt   int64           // instant the entry price represents (follower), 0 if unknown
	OutcomeObservedAt int64           // instant the horizon price represents (candle close), 0 if unknown
	ChasePct          float64         // (BasePrice/LeaderPrice - 1) * 100 — how much the
	// price moved before we could enter (follower rows; ~0 for leader)
}

// MarkoutsAt returns all filled markouts of one kind at one horizon, joined
// with their events. ChasePct is computed in Go (base vs leader price).
func (s *Store) MarkoutsAt(kind string, horizon time.Duration) ([]MarkoutStat, error) {
	rows, err := s.db.Query(`
		SELECT m.event_id, e.wallet, e.wallet_type, e.side, e.amount_usd,
		       e.trade_time, e.received_at, m.horizon_ms, m.return_pct,
		       m.base_price, e.price_usd, m.entry_observed_at, m.outcome_observed_at
		FROM markouts m
		JOIN trade_events e ON e.event_id = m.event_id
		WHERE m.kind = ? AND m.horizon_ms = ? AND m.observed_price IS NOT NULL`,
		kind, horizon.Milliseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MarkoutStat
	for rows.Next() {
		var m MarkoutStat
		var hms int64
		var base sql.NullFloat64
		var eoa sql.NullInt64
		var ooa sql.NullInt64
		if err := rows.Scan(&m.EventID, &m.Wallet, &m.WalletType, &m.Side, &m.AmountUSD,
			&m.TradeTime, &m.ReceivedAt, &hms, &m.ReturnPct, &base, &m.LeaderPrice,
			&eoa, &ooa); err != nil {
			return nil, err
		}
		m.EntryObservedAt = eoa.Int64
		m.OutcomeObservedAt = ooa.Int64
		m.Horizon = time.Duration(hms) * time.Millisecond
		m.BasePrice = base.Float64
		if m.BasePrice > 0 && m.LeaderPrice > 0 {
			m.ChasePct = (m.BasePrice/m.LeaderPrice - 1) * 100
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CensusRow is one DUE markout row regardless of fill status — the input for
// coverage-aware EV (selection bias guard).
type CensusRow struct {
	EventID       string
	WalletType    string
	TradeTime     int64
	ReceivedAt    int64
	BaseMs        int64
	HorizonMs     int64
	ObservedPrice *float64
	Status        string
	BasePrice     float64
	LeaderPrice   float64
	Side          string
}

// MarkoutCensus returns every DUE row of one kind at one horizon, filled or
// not. Rows whose sampling time (plus grace) has not passed are excluded —
// counting them would dilute coverage with rows that are simply not ripe yet.
func (s *Store) MarkoutCensus(kind string, horizon, grace time.Duration, now time.Time) ([]CensusRow, error) {
	cutoff := now.Add(-grace).Unix()
	rows, err := s.db.Query(`
		SELECT m.event_id, e.wallet_type, e.trade_time, e.received_at,
		       m.base_ms, m.horizon_ms, m.observed_price, m.status,
		       m.base_price, e.price_usd, e.side
		FROM markouts m
		JOIN trade_events e ON e.event_id = m.event_id
		WHERE m.kind = ? AND m.horizon_ms = ?
		  AND m.base_ms + m.horizon_ms/1000 <= ?`,
		kind, horizon.Milliseconds(), cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CensusRow
	for rows.Next() {
		var r CensusRow
		var obs sql.NullFloat64
		var base sql.NullFloat64
		if err := rows.Scan(&r.EventID, &r.WalletType, &r.TradeTime, &r.ReceivedAt,
			&r.BaseMs, &r.HorizonMs, &obs, &r.Status, &base, &r.LeaderPrice,
			&r.Side); err != nil {
			return nil, err
		}
		if obs.Valid {
			r.ObservedPrice = &obs.Float64
		}
		r.BasePrice = base.Float64
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkoutStatusCounts groups rows whose horizon has passed by status, plus
// the number still pending (not yet due).
func (s *Store) MarkoutStatusCounts(kind string, horizon, grace time.Duration, now time.Time) (map[string]int, int, error) {
	cutoff := now.Add(-grace).Unix()
	rows, err := s.db.Query(`
		SELECT status, COUNT(*) FROM markouts
		WHERE kind = ? AND horizon_ms = ? AND base_ms + horizon_ms/1000 <= ?
		GROUP BY status`,
		kind, horizon.Milliseconds(), cutoff)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, 0, err
		}
		counts[st] = n
	}
	var pending int
	err = s.db.QueryRow(`
		SELECT COUNT(*) FROM markouts
		WHERE kind = ? AND horizon_ms = ? AND base_ms + horizon_ms/1000 > ?
		  AND status = ?`,
		kind, horizon.Milliseconds(), cutoff, MarkoutStatusPending).Scan(&pending)
	if err != nil {
		return nil, 0, err
	}
	return counts, pending, rows.Err()
}

// DueCoverage counts filled vs DUE rows for one kind at one horizon — the
// fraction of due rows that actually got a price. It is the v0.1 proxy for
// survival/retention: a row stays NULL when the token stopped trading before
// its horizon, so filled/due ≈ "price still available at H".
func (s *Store) DueCoverage(kind string, horizon time.Duration, grace time.Duration, now time.Time) (filled, due int64, err error) {
	cutoff := now.Add(-grace).Unix()
	err = s.db.QueryRow(`
		SELECT COUNT(*) FROM markouts
		WHERE kind = ? AND horizon_ms = ?
		  AND base_ms + horizon_ms/1000 <= ?`,
		kind, horizon.Milliseconds(), cutoff).Scan(&due)
	if err != nil {
		return
	}
	err = s.db.QueryRow(`
		SELECT COUNT(*) FROM markouts
		WHERE kind = ? AND horizon_ms = ?
		  AND base_ms + horizon_ms/1000 <= ?
		  AND observed_price IS NOT NULL`,
		kind, horizon.Milliseconds(), cutoff).Scan(&filled)
	return
}

// MarkoutCoverage counts filled vs total rows at a horizon for one kind.
func (s *Store) MarkoutCoverage(kind string, horizon time.Duration) (filled, total int64, err error) {
	err = s.db.QueryRow(
		`SELECT COUNT(*) FROM markouts WHERE kind = ? AND horizon_ms = ?`,
		kind, horizon.Milliseconds()).Scan(&total)
	if err != nil {
		return
	}
	err = s.db.QueryRow(
		`SELECT COUNT(*) FROM markouts WHERE kind = ? AND horizon_ms = ? AND observed_price IS NOT NULL`,
		kind, horizon.Milliseconds()).Scan(&filled)
	return
}

// ReplicationRow is one wallet's coverage-aware replication census at one
// horizon: EVERY due follower row bucketed, so survivor bias is visible —
// a wallet with 10 great fills and 90 dead tokens shows coverage 10% and a
// conservative EV dragged down by market loss, instead of a flattering
// filled-only mean.
type ReplicationRow struct {
	Wallet     string
	Due        int
	Filled     int
	MarketLoss int // no_candle + token_inactive + stale_outcome
	MeasLoss   int // api_error + rate_limited + lookback_miss + price_parse_error + no_kline_data
	Unresolved int // due but still pending (worker lag)
	// ObservedEV = mean return of filled rows; Valid=false when no fills.
	ObservedEV    float64
	ObservedValid bool
}

// ReplicationCensus aggregates one horizon's due follower markouts per
// wallet. Conservative EV is computed by the caller (it needs noExitLoss).
func (s *Store) ReplicationCensus(horizon, grace time.Duration, now time.Time) ([]ReplicationRow, error) {
	cutoff := now.Add(-grace).Unix()
	rows, err := s.db.Query(`
		SELECT e.wallet,
		       COUNT(*) AS due,
		       SUM(CASE WHEN m.observed_price IS NOT NULL AND m.base_price > 0 THEN 1 ELSE 0 END) AS filled,
		       SUM(CASE WHEN m.status IN ('no_candle','token_inactive','stale_outcome') THEN 1 ELSE 0 END) AS market_loss,
		       SUM(CASE WHEN m.status IN ('api_error','rate_limited','lookback_miss','price_parse_error','no_kline_data') THEN 1 ELSE 0 END) AS meas_loss,
		       SUM(CASE WHEN m.status = 'pending' THEN 1 ELSE 0 END) AS unresolved,
		       AVG(CASE WHEN m.observed_price IS NOT NULL AND m.base_price > 0
		                THEN (m.observed_price / m.base_price - 1) * 100 END) AS obs_ev
		FROM markouts m
		JOIN trade_events e ON e.event_id = m.event_id
		WHERE m.kind = 'follower' AND m.horizon_ms = ?
		  AND m.base_ms + m.horizon_ms/1000 <= ?
		GROUP BY e.wallet
		ORDER BY e.wallet`, horizon.Milliseconds(), cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReplicationRow
	for rows.Next() {
		var r ReplicationRow
		var obsEV sql.NullFloat64
		if err := rows.Scan(&r.Wallet, &r.Due, &r.Filled, &r.MarketLoss,
			&r.MeasLoss, &r.Unresolved, &obsEV); err != nil {
			return nil, err
		}
		if obsEV.Valid {
			r.ObservedEV = obsEV.Float64
			r.ObservedValid = true
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
