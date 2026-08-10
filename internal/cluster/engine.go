// Package cluster maintains per-token rolling windows of distinct-wallet
// convergence. It is the first layer of alpha: it answers "how many smart
// wallets piled into this token recently", nothing more.
package cluster

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/auren23/followedge/internal/domain"
	"github.com/auren23/followedge/internal/storage"
)

// Engine recomputes rolling-window features from the DB every time an event
// lands and appends a cluster_sample row per window. DB-driven recompute is
// deliberately simple: restart-safe, no in-memory state to drift, and cheap
// at meme-coin event rates.
type Engine struct {
	store   *storage.Store
	windows []time.Duration
	log     *slog.Logger
}

func NewEngine(store *storage.Store, windows []time.Duration) *Engine {
	sorted := append([]time.Duration(nil), windows...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return &Engine{store: store, windows: sorted, log: slog.With("pkg", "cluster")}
}

// OnEvent recomputes all windows for the event's token and persists samples.
func (e *Engine) OnEvent(ctx context.Context, ev domain.TradeEvent) error {
	now := time.Now().UTC()
	maxW := e.windows[len(e.windows)-1]
	recent, err := e.store.RecentEvents(ev.TokenAddress, now.Add(-maxW))
	if err != nil {
		return err
	}

	for _, w := range e.windows {
		f := domain.ClusterFeatures{TokenAddress: ev.TokenAddress, Window: w}
		cutoff := now.Add(-w).Unix()

		smartBuy := map[string]bool{}
		smartSell := map[string]bool{}
		kolBuy := map[string]bool{}
		kolSell := map[string]bool{}
		var minTs, maxTs int64

		for _, r := range recent {
			if r.TradeTime < cutoff {
				break // recent is sorted desc
			}
			f.EventCount++
			if minTs == 0 || r.TradeTime < minTs {
				minTs = r.TradeTime
			}
			if r.TradeTime > maxTs {
				maxTs = r.TradeTime
			}
			switch {
			case r.WalletType == string(domain.WalletSmartMoney) && r.Side == string(domain.Buy):
				smartBuy[r.Wallet] = true
				f.SmartBuyUSD += r.AmountUSD
			case r.WalletType == string(domain.WalletSmartMoney) && r.Side == string(domain.Sell):
				smartSell[r.Wallet] = true
				f.SmartSellUSD += r.AmountUSD
			case r.WalletType == string(domain.WalletKOL) && r.Side == string(domain.Buy):
				kolBuy[r.Wallet] = true
			case r.WalletType == string(domain.WalletKOL) && r.Side == string(domain.Sell):
				kolSell[r.Wallet] = true
			}
		}
		f.SmartBuyWallets = len(smartBuy)
		f.SmartSellWallets = len(smartSell)
		f.KOLBuyWallets = len(kolBuy)
		f.KOLSellWallets = len(kolSell)
		f.FirstEventAt = time.Unix(minTs, 0).UTC()
		f.LastEventAt = time.Unix(maxTs, 0).UTC()

		if err := e.store.InsertClusterSample(f); err != nil {
			e.log.Warn("sample insert failed", "token", ev.TokenAddress, "window", w, "err", err)
		}
	}
	return nil
}
