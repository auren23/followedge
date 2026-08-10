package storage

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/auren23/followedge/internal/domain"
)

// CreateMarkouts reserves a markout row for every horizon of a new event.
func (s *Store) CreateMarkouts(e domain.TradeEvent, horizons []time.Duration, createdAt time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, h := range horizons {
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO markouts (event_id, horizon_ms, base_price, created_at) VALUES (?,?,?,?)`,
			e.ID, h.Milliseconds(), e.PriceUSD, createdAt.Unix()); err != nil {
			return fmt.Errorf("create markout %v: %w", h, err)
		}
	}
	return tx.Commit()
}

// DueMarkout is one (event, horizon) waiting for its price sample.
type DueMarkout struct {
	EventID   string
	Token     string
	Horizon   time.Duration
	BasePrice float64
	DueAt     time.Time // TradeTime + horizon
}

// DueMarkouts lists markouts whose sampling time (plus grace) has passed.
func (s *Store) DueMarkouts(grace time.Duration, now time.Time, limit int) ([]DueMarkout, error) {
	cutoff := now.Add(-grace).Unix()
	rows, err := s.db.Query(`
		SELECT m.event_id, m.horizon_ms, m.base_price,
		       e.token_address, e.trade_time
		FROM markouts m
		JOIN trade_events e ON e.event_id = m.event_id
		WHERE m.observed_price IS NULL
		  AND e.trade_time + m.horizon_ms/1000 <= ?
		ORDER BY e.trade_time + m.horizon_ms/1000
		LIMIT ?`, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DueMarkout
	for rows.Next() {
		var d DueMarkout
		var hms, tt int64
		if err := rows.Scan(&d.EventID, &hms, &d.BasePrice, &d.Token, &tt); err != nil {
			return nil, err
		}
		d.Horizon = time.Duration(hms) * time.Millisecond
		d.DueAt = time.Unix(tt, 0).Add(d.Horizon)
		out = append(out, d)
	}
	return out, rows.Err()
}

// FillMarkout writes the sampled price and return for one markout.
func (s *Store) FillMarkout(eventID string, horizon time.Duration, observed float64) error {
	_, err := s.db.Exec(`
		UPDATE markouts SET observed_price = ?,
			return_pct = CASE WHEN base_price > 0 THEN ( ? / base_price - 1 ) * 100 ELSE NULL END
		WHERE event_id = ? AND horizon_ms = ? AND observed_price IS NULL`,
		observed, observed, eventID, horizon.Milliseconds())
	return err
}

// MarkoutStat is one analyzed markout (for chase / wallet analysis).
type MarkoutStat struct {
	EventID    string
	Wallet     string
	WalletType string
	Side       string
	AmountUSD  float64
	TradeTime  int64
	Horizon    time.Duration
	ReturnPct  sql.NullFloat64
	BasePrice  float64
}

// MarkoutsAt returns all filled markouts at one horizon with their events.
func (s *Store) MarkoutsAt(horizon time.Duration) ([]MarkoutStat, error) {
	rows, err := s.db.Query(`
		SELECT m.event_id, e.wallet, e.wallet_type, e.side, e.amount_usd, e.trade_time,
		       m.horizon_ms, m.return_pct, m.base_price
		FROM markouts m
		JOIN trade_events e ON e.event_id = m.event_id
		WHERE m.horizon_ms = ? AND m.observed_price IS NOT NULL`,
		horizon.Milliseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MarkoutStat
	for rows.Next() {
		var m MarkoutStat
		var hms int64
		if err := rows.Scan(&m.EventID, &m.Wallet, &m.WalletType, &m.Side, &m.AmountUSD, &m.TradeTime,
			&hms, &m.ReturnPct, &m.BasePrice); err != nil {
			return nil, err
		}
		m.Horizon = time.Duration(hms) * time.Millisecond
		out = append(out, m)
	}
	return out, rows.Err()
}

// MarkoutCoverage counts filled vs total markouts at a horizon.
func (s *Store) MarkoutCoverage(horizon time.Duration) (filled, total int64, err error) {
	err = s.db.QueryRow(
		`SELECT COUNT(*) FROM markouts WHERE horizon_ms = ?`, horizon.Milliseconds()).Scan(&total)
	if err != nil {
		return
	}
	err = s.db.QueryRow(
		`SELECT COUNT(*) FROM markouts WHERE horizon_ms = ? AND observed_price IS NOT NULL`,
		horizon.Milliseconds()).Scan(&filled)
	return
}
