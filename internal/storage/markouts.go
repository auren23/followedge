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
func (s *Store) DueMarkouts(grace time.Duration, now time.Time, limit int) ([]DueMarkout, error) {
	cutoff := now.Add(-grace).Unix()
	// follower rows first (the measurement that matters), then newest first —
	// stale leader rows from dead tokens must not starve fresh data.
	rows, err := s.db.Query(`
		SELECT m.event_id, m.kind, m.horizon_ms, m.base_price, m.base_ms,
		       e.token_address
		FROM markouts m
		JOIN trade_events e ON e.event_id = m.event_id
		WHERE m.observed_price IS NULL
		  AND m.base_ms + m.horizon_ms/1000 <= ?
		ORDER BY (m.kind = 'follower') DESC, m.base_ms + m.horizon_ms/1000 DESC
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

// SetEntryPrice fills a follower row's base_price with the price at ReceivedAt
// (the entry a real follower would have paid). No-op if already set.
func (s *Store) SetEntryPrice(eventID, kind string, horizon time.Duration, entryPrice float64) error {
	_, err := s.db.Exec(`
		UPDATE markouts SET base_price = ?
		WHERE event_id = ? AND kind = ? AND horizon_ms = ? AND base_price IS NULL`,
		entryPrice, eventID, kind, horizon.Milliseconds())
	return err
}

// FillMarkout writes the sampled horizon price and return for one row.
func (s *Store) FillMarkout(eventID, kind string, horizon time.Duration, observed float64) error {
	_, err := s.db.Exec(`
		UPDATE markouts SET observed_price = ?,
			return_pct = CASE WHEN base_price > 0 THEN ( ? / base_price - 1 ) * 100 ELSE NULL END
		WHERE event_id = ? AND kind = ? AND horizon_ms = ? AND observed_price IS NULL`,
		observed, observed, eventID, kind, horizon.Milliseconds())
	return err
}

// MarkoutStat is one analyzed markout, with the chase number attached.
type MarkoutStat struct {
	EventID     string
	Wallet      string
	WalletType  string
	Side        string
	AmountUSD   float64
	TradeTime   int64
	ReceivedAt  int64
	Horizon     time.Duration
	ReturnPct   sql.NullFloat64 // follower/leader forward return at horizon
	BasePrice   float64         // entry price (leader: price_usd; follower: ReceivedAt price)
	LeaderPrice float64         // leader's price_usd, for chase
	ChasePct    float64         // (BasePrice/LeaderPrice - 1) * 100 — how much the
	// price moved before we could enter (follower rows; ~0 for leader)
}

// MarkoutsAt returns all filled markouts of one kind at one horizon, joined
// with their events. ChasePct is computed in Go (base vs leader price).
func (s *Store) MarkoutsAt(kind string, horizon time.Duration) ([]MarkoutStat, error) {
	rows, err := s.db.Query(`
		SELECT m.event_id, e.wallet, e.wallet_type, e.side, e.amount_usd,
		       e.trade_time, e.received_at, m.horizon_ms, m.return_pct,
		       m.base_price, e.price_usd
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
		if err := rows.Scan(&m.EventID, &m.Wallet, &m.WalletType, &m.Side, &m.AmountUSD,
			&m.TradeTime, &m.ReceivedAt, &hms, &m.ReturnPct, &base, &m.LeaderPrice); err != nil {
			return nil, err
		}
		m.Horizon = time.Duration(hms) * time.Millisecond
		m.BasePrice = base.Float64
		if m.BasePrice > 0 && m.LeaderPrice > 0 {
			m.ChasePct = (m.BasePrice/m.LeaderPrice - 1) * 100
		}
		out = append(out, m)
	}
	return out, rows.Err()
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
