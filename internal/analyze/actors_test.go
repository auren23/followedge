package analyze

import (
	"testing"

	"github.com/auren23/followedge/internal/storage"
)

// actorRows is pure aggregation — test it without a database.

func TestActorRowsAggregates(t *testing.T) {
	groups := []storage.ActorGroup{
		// Wallet A: +$120 on T1, +$80 on T2 (both tokens), profitable both days
		{Wallet: "A", WalletType: "smart_money", Token: "TOK1", Day: "2026-04-01", RealizedPnL: 60, Trades: 4, Buys: 2, Sells: 2, TotalUSD: 1000},
		{Wallet: "A", WalletType: "smart_money", Token: "TOK2", Day: "2026-04-01", RealizedPnL: 60, Trades: 4, Buys: 2, Sells: 2, TotalUSD: 1000},
		{Wallet: "A", WalletType: "smart_money", Token: "TOK1", Day: "2026-04-02", RealizedPnL: 80, Trades: 3, Buys: 2, Sells: 1, TotalUSD: 800},
		// Wallet B: lottery — $300 pnl on one token, $3 elsewhere; losing day 2
		{Wallet: "B", WalletType: "smart_money", Token: "LOTTO", Day: "2026-04-01", RealizedPnL: 300, Trades: 5, Buys: 3, Sells: 2, TotalUSD: 500},
		{Wallet: "B", WalletType: "smart_money", Token: "OTHER", Day: "2026-04-01", RealizedPnL: 3, Trades: 3, Buys: 2, Sells: 1, TotalUSD: 100},
		{Wallet: "B", WalletType: "smart_money", Token: "LOTTO", Day: "2026-04-02", RealizedPnL: -10, Trades: 2, Buys: 1, Sells: 1, TotalUSD: 60},
	}
	actors := actorRows(groups, map[string][]float64{})

	a := actors["A"]
	if a == nil || a.RealizedPnL != 200 || a.Trades != 11 || a.Buys != 6 || a.Sells != 5 {
		t.Fatalf("actor A aggregated wrong: %+v", a)
	}
	if a.ActiveDays != 2 || a.ProfitableDays != 2 {
		t.Errorf("A consistency = %d/%d, want 2/2", a.ProfitableDays, a.ActiveDays)
	}
	if a.Top1Share != 0.70 { // TOK1: 60+80=140 of 200
		t.Errorf("A top1 share = %.2f, want 0.70", a.Top1Share)
	}

	b := actors["B"]
	if b == nil || b.RealizedPnL != 293 {
		t.Fatalf("actor B aggregated wrong: %+v", b)
	}
	if b.Top1Share != 290.0/293.0 { // LOTTO net 290 incl. losing day
		t.Errorf("B top1 share = %.2f, want %.2f", b.Top1Share, 290.0/293.0)
	}
	if b.ActiveDays != 2 || b.ProfitableDays != 1 {
		t.Errorf("B consistency = %d/%d, want 1/2", b.ProfitableDays, b.ActiveDays)
	}
	// B's drawdown: cumulative 303 → 293, maxDD = 0 (peak never declined).
	// A has no decline either, so drawdown scores are equal — the separation
	// must come from consistency + concentration.
	if b.Quality >= a.Quality {
		t.Errorf("lottery actor B quality %.1f should be below consistent actor A %.1f", b.Quality, a.Quality)
	}
}

func TestReplicabilityZeroForLosingEdge(t *testing.T) {
	if got := replicabilityScore(-3, 50); got != 0 {
		t.Errorf("negative EV must score 0, got %.1f", got)
	}
	if got := replicabilityScore(0, 50); got != 0 {
		t.Errorf("zero EV must score 0, got %.1f", got)
	}
	// +5% mean over 20 fills → half of max
	if got := replicabilityScore(5, 20); got < 49 || got > 51 {
		t.Errorf("+5%%/20 fills should be ~50, got %.1f", got)
	}
	// tiny sample is heavily discounted
	if got := replicabilityScore(10, 2); got != 10 {
		t.Errorf("+10%% with 2 fills should be 10 (sample factor 0.1), got %.1f", got)
	}
}
