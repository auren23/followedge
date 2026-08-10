package mechanism

import (
	"math"
	"testing"

	"github.com/auren23/followedge/internal/storage"
)

// TestBuildProfile pins the v0.2.0.1 semantics: reentry rate (not buy
// count), real partial exits (PartialExitLegs, not data-gap status),
// closed-only pnl stats, per-feature sample sizes, and n/a on missing data.
func TestBuildProfile(t *testing.T) {
	episodes := []storage.Episode{
		// closed, profitable: one add, one REAL partial exit (s1), close at 180s
		{Wallet: "W", Token: "T1", OpenedAt: 100, ClosedAt: 280, Adds: 1, Reduces: 2,
			CapitalIn: 150, CapitalOut: 200, RealizedPnL: 50, HoldDurationS: 180,
			Status:      storage.EpisodeClosed,
			FirstSellAt: 160, InitialBuyUSD: 100, AddBuyUSD: 50,
			SellLegs: 2, PartialExitLegs: 1, DataGap: false},
		// closed, losing, no partial exit
		{Wallet: "W", Token: "T2", OpenedAt: 500, ClosedAt: 800, Adds: 0, Reduces: 1,
			CapitalIn: 200, CapitalOut: 100, RealizedPnL: -100, HoldDurationS: 300,
			Status:      storage.EpisodeClosed,
			FirstSellAt: 800, InitialBuyUSD: 200, AddBuyUSD: 0,
			SellLegs: 1, PartialExitLegs: 0, DataGap: false},
		// data-gap (partial status): sells exceeded visible position — NOT
		// a partial exit; excluded from closed pnl
		{Wallet: "W", Token: "T3", OpenedAt: 900, ClosedAt: 960, Adds: 0, Reduces: 1,
			CapitalIn: 50, CapitalOut: 80, RealizedPnL: 30, HoldDurationS: 60,
			Status:      storage.EpisodePartial,
			FirstSellAt: 960, InitialBuyUSD: 0, AddBuyUSD: 0,
			SellLegs: 1, PartialExitLegs: 0, DataGap: true},
		// open (never exited) — SAME token T1 as the first episode: re-entry
		{Wallet: "W", Token: "T1", OpenedAt: 1000, ClosedAt: 0, Adds: 0, Reduces: 0,
			CapitalIn: 30, CapitalOut: 0, RealizedPnL: 0, HoldDurationS: 0,
			Status:      storage.EpisodeOpen,
			FirstSellAt: 0, InitialBuyUSD: 30, AddBuyUSD: 0,
			SellLegs: 0, PartialExitLegs: 0, DataGap: false},
	}
	// 4 buys: T1×3 (one add + one re-entry), T2 — reentry rate over EPISODES:
	// 4 episodes, 3 distinct tokens → 1 - 3/4 = 0.25
	entries := []storage.EntryRow{
		{Token: "T1", AmountUSD: 100, TradeTime: 100, ReceivedAt: 130},  // age 30s
		{Token: "T1", AmountUSD: 50, TradeTime: 150, ReceivedAt: 190},   // age 40s
		{Token: "T2", AmountUSD: 200, TradeTime: 500, ReceivedAt: 560},  // age 60s
		{Token: "T1", AmountUSD: 30, TradeTime: 1000, ReceivedAt: 1010}, // age 10s
	}
	chases := []float64{1.0, 2.0, 3.0, 4.0, 5.0}
	smartPrior := []float64{1, 2, 3, 4, 5}
	kolPrior := []float64{0, 0, 1, 1, 2}

	p := BuildProfile("W", episodes, entries, chases, smartPrior, kolPrior)

	// ENTRY
	if p.Entry.Count != 4 || math.Abs(p.Entry.ReentryRate-0.25) > 0.001 {
		t.Errorf("entry count/reentry = %d/%.2f, want 4/0.25", p.Entry.Count, p.Entry.ReentryRate)
	}
	if p.Entry.MedianInitialBuy.Value != 65 || p.Entry.MedianInitialBuy.N != 4 { // [100,200,0,30] → sorted → (30+100)/2
		t.Errorf("initial buy = %.0f (n%d), want 65 (n4)", p.Entry.MedianInitialBuy.Value, p.Entry.MedianInitialBuy.N)
	}
	if p.Entry.MedianChase.Value != 3 || p.Entry.MedianChase.N != 5 {
		t.Errorf("median chase = %.0f (n%d), want 3 (n5)", p.Entry.MedianChase.Value, p.Entry.MedianChase.N)
	}
	if p.Entry.SmartPriorP50.Value != 3 || p.Entry.PriorFlowN != 5 {
		t.Errorf("prior smart = %.0f (n%d), want 3 (n5)", p.Entry.SmartPriorP50.Value, p.Entry.PriorFlowN)
	}
	if math.Abs(p.Entry.Cluster3Plus-0.60) > 0.001 { // 3,4,5 of 5 >= 3
		t.Errorf("cluster3plus = %.2f, want 0.60", p.Entry.Cluster3Plus)
	}

	// POSITION — hold median only over closed (180, 300)
	if p.Position.Episodes != 4 {
		t.Errorf("episodes = %d, want 4", p.Position.Episodes)
	}
	if p.Position.MedianHoldSecs.Value != 240 || p.Position.MedianHoldSecs.N != 2 {
		t.Errorf("median hold = %.0f (n%d), want 240 (n2, closed only)", p.Position.MedianHoldSecs.Value, p.Position.MedianHoldSecs.N)
	}

	// EXIT — real partial exits: only T1 (1 of 2 observable episodes)
	if math.Abs(p.Exit.PartialExitRatio-0.50) > 0.001 || p.Exit.PartialExitN != 1 || p.Exit.ObservableN != 2 {
		t.Errorf("partial exits = %.2f (%d of %d), want 0.50 (1 of 2)", p.Exit.PartialExitRatio, p.Exit.PartialExitN, p.Exit.ObservableN)
	}
	if p.Exit.FirstSellP50.Value != 180 || p.Exit.FirstSellP50.N != 2 { // 160-100=60, 800-500=300 → 180
		t.Errorf("first sell P50 = %.0f (n%d), want 180 (n2)", p.Exit.FirstSellP50.Value, p.Exit.FirstSellP50.N)
	}
	if p.Exit.CloseP50.Value != 240 || p.Exit.CloseP50.N != 2 {
		t.Errorf("close P50 = %.0f (n%d), want 240 (n2)", p.Exit.CloseP50.Value, p.Exit.CloseP50.N)
	}
	// closed-only pnl: 50 + (-100) = -50; the data-gap +30 shown separately
	if p.Exit.ClosedPnl != -50 || math.Abs(p.Exit.ClosedWinRate-0.50) > 0.001 {
		t.Errorf("closed pnl/win = %.0f/%.2f, want -50/0.50", p.Exit.ClosedPnl, p.Exit.ClosedWinRate)
	}
	if math.Abs(p.Exit.IncompleteRatio-0.25) > 0.001 || p.Exit.IncompletePnl != 30 {
		t.Errorf("incomplete = %.2f pnl %.0f, want 0.25/30", p.Exit.IncompleteRatio, p.Exit.IncompletePnl)
	}

	// missing data → n/a (N=0), not a fabricated zero
	empty := BuildProfile("W", nil, nil, nil, nil, nil)
	if empty.Entry.MedianChase.N != 0 || empty.Entry.MedianAge.N != 0 ||
		empty.Exit.FirstSellP50.N != 0 || empty.Exit.CloseP50.N != 0 {
		t.Errorf("empty profile must carry N=0, got %+v", empty)
	}
}
