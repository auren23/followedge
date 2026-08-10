package mechanism

import (
	"math"
	"testing"

	"github.com/auren23/followedge/internal/storage"
)

// TestBuildProfile pins the behavior profile facts: token reuse, medians,
// cluster share, exit ratios and PnL aggregation.
func TestBuildProfile(t *testing.T) {
	episodes := []storage.Episode{
		// closed, profitable, one add, one reduce, hold 100s
		{Wallet: "W", Token: "T1", OpenedAt: 100, ClosedAt: 200, Adds: 1, Reduces: 1,
			CapitalIn: 100, CapitalOut: 150, RealizedPnL: 50, HoldDurationS: 100, Status: storage.EpisodeClosed},
		// closed, losing, hold 300s
		{Wallet: "W", Token: "T2", OpenedAt: 500, ClosedAt: 800, Adds: 2, Reduces: 1,
			CapitalIn: 200, CapitalOut: 100, RealizedPnL: -100, HoldDurationS: 300, Status: storage.EpisodeClosed},
		// partial (data gap), hold 60s
		{Wallet: "W", Token: "T3", OpenedAt: 900, ClosedAt: 960, Adds: 0, Reduces: 2,
			CapitalIn: 50, CapitalOut: 80, RealizedPnL: 30, HoldDurationS: 60, Status: storage.EpisodePartial},
		// open
		{Wallet: "W", Token: "T4", OpenedAt: 1000, ClosedAt: 0, Adds: 0, Reduces: 0,
			CapitalIn: 30, CapitalOut: 0, RealizedPnL: 0, HoldDurationS: 0, Status: storage.EpisodeOpen},
	}
	// 5 entries: T1×2 (reuse), T2, T3, T4 → reuse = 1 - 4/5 = 0.20
	entries := []storage.EntryRow{
		{Token: "T1", AmountUSD: 100, TradeTime: 100, ReceivedAt: 130},   // age 30s
		{Token: "T1", AmountUSD: 50, TradeTime: 150, ReceivedAt: 190},    // age 40s
		{Token: "T2", AmountUSD: 200, TradeTime: 500, ReceivedAt: 560},   // age 60s
		{Token: "T3", AmountUSD: 300, TradeTime: 900, ReceivedAt: 960},   // age 60s
		{Token: "T4", AmountUSD: 400, TradeTime: 1000, ReceivedAt: 1010}, // age 10s
	}
	firstSells := []int64{60, 150, 30}
	chases := []float64{1.0, 2.0, 3.0, 4.0, 5.0}
	smartPrior := []float64{1, 2, 3, 4, 5}
	kolPrior := []float64{0, 0, 1, 1, 2}

	p := BuildProfile("W", episodes, entries, firstSells, chases, smartPrior, kolPrior)

	// ENTRY
	if p.Entry.Count != 5 || math.Abs(p.Entry.TokenReuse-0.20) > 0.001 {
		t.Errorf("entry count/reuse = %d/%.2f, want 5/0.20", p.Entry.Count, p.Entry.TokenReuse)
	}
	if p.Entry.MedianSize != 200 { // sorted: 50,100,200,300,400
		t.Errorf("median size = %.0f, want 200", p.Entry.MedianSize)
	}
	if p.Entry.MedianAge != 40 { // sorted: 10,30,40,60,60
		t.Errorf("median age = %.0f, want 40", p.Entry.MedianAge)
	}
	if p.Entry.MedianChase != 3 || p.Entry.SmartPriorP50 != 3 {
		t.Errorf("median chase/smart = %.0f/%.0f, want 3/3", p.Entry.MedianChase, p.Entry.SmartPriorP50)
	}
	if math.Abs(p.Entry.Cluster3Plus-0.60) > 0.001 { // 3,4,5 of 5 >= 3
		t.Errorf("cluster3plus = %.2f, want 0.60", p.Entry.Cluster3Plus)
	}

	// POSITION
	if p.Position.Episodes != 4 {
		t.Errorf("episodes = %d, want 4", p.Position.Episodes)
	}
	if p.Position.MedianAdds != 0.5 { // sorted: 0,0,1,2
		t.Errorf("median adds = %.1f, want 0.5", p.Position.MedianAdds)
	}
	if p.Position.MedianHoldSecs != 100 { // closed+partial: 60,100,300
		t.Errorf("median hold = %.0f, want 100", p.Position.MedianHoldSecs)
	}

	// EXIT
	if math.Abs(p.Exit.PartialRatio-0.25) > 0.001 || math.Abs(p.Exit.FullRatio-0.50) > 0.001 {
		t.Errorf("exit ratios = partial %.2f / full %.2f, want 0.25/0.50", p.Exit.PartialRatio, p.Exit.FullRatio)
	}
	if p.Exit.MedianFirstSellSecs != 60 { // sorted: 30,60,150
		t.Errorf("first sell P50 = %.0f, want 60", p.Exit.MedianFirstSellSecs)
	}
	if p.Exit.MedianCloseSecs != 100 { // closed: 100,300
		t.Errorf("close P50 = %.0f, want 100", p.Exit.MedianCloseSecs)
	}
	if p.Exit.TotalPnl != -20 { // 50 - 100 + 30
		t.Errorf("total pnl = %.0f, want -20", p.Exit.TotalPnl)
	}
	if math.Abs(p.Exit.ProfitableRatio-2.0/3.0) > 0.001 { // 2 of 3 realized (closed+partial) profitable
		t.Errorf("profitable ratio = %.2f, want %.2f", p.Exit.ProfitableRatio, 2.0/3.0)
	}
}
