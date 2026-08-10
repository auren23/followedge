package storage

import (
	"time"

	"github.com/auren23/followedge/internal/domain"
)

// InsertClusterSample appends the rolling cluster state for (token, window)
// at the moment an event landed. Append-only: analysis reads history, not a
// mutable "current" row.
func (s *Store) InsertClusterSample(c domain.ClusterFeatures) error {
	_, err := s.db.Exec(`
		INSERT INTO cluster_samples
		(token_address, window_ms, ts,
		 smart_buy_wallets, smart_sell_wallets, kol_buy_wallets, kol_sell_wallets,
		 smart_buy_usd, smart_sell_usd, net_smart_flow_usd, event_count)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		c.TokenAddress, c.Window.Milliseconds(), c.LastEventAt.Unix(),
		c.SmartBuyWallets, c.SmartSellWallets, c.KOLBuyWallets, c.KOLSellWallets,
		c.SmartBuyUSD, c.SmartSellUSD, c.NetSmartFlowUSD(), c.EventCount)
	return err
}

// ClusterSampleRow is one persisted rolling-window snapshot.
type ClusterSampleRow struct {
	TokenAddress     string
	Window           time.Duration
	TS               int64
	SmartBuyWallets  int
	SmartSellWallets int
	KOLBuyWallets    int
	KOLSellWallets   int
	NetFlowUSD       float64
	EventCount       int
}

// ClusterSamples returns the most recent snapshot per token for one window
// (the converged state right now), plus optional recency filter.
func (s *Store) ClusterSamples(window time.Duration, since time.Time) ([]ClusterSampleRow, error) {
	rows, err := s.db.Query(`
		SELECT token_address, window_ms, ts,
		       smart_buy_wallets, smart_sell_wallets, kol_buy_wallets, kol_sell_wallets,
		       net_smart_flow_usd, event_count
		FROM cluster_samples
		WHERE window_ms = ? AND ts >= ?
		ORDER BY ts DESC`, window.Milliseconds(), since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ClusterSampleRow
	for rows.Next() {
		var r ClusterSampleRow
		var wms int64
		if err := rows.Scan(&r.TokenAddress, &wms, &r.TS,
			&r.SmartBuyWallets, &r.SmartSellWallets, &r.KOLBuyWallets, &r.KOLSellWallets,
			&r.NetFlowUSD, &r.EventCount); err != nil {
			return nil, err
		}
		r.Window = time.Duration(wms) * time.Millisecond
		out = append(out, r)
	}
	return out, rows.Err()
}
