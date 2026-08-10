package mechanism

import (
	"math"
	"testing"

	"github.com/auren23/followedge/internal/storage"
)

// TestBuildProfile pins the v0.2.0.2 semantics: initial vs add context
// separation, data-gap exclusion from size medians, real partial exits
// (PartialExitLegs), closed-only pnl, prior-flow validity, and n/a on
// missing data.
func TestBuildProfile(t *testing.T) {
	episodes := []storage.Episode{
		// closed, profitable: one add, one REAL partial exit (s1), close at 180s
		{Wallet: "W", Token: "T1", OpenedAt: 100, ClosedAt: 280, Adds: 1, Reduces: 2,
			CapitalIn: 150, CapitalOut: 200, RealizedPnL: 50, HoldDurationS: 180,
			Status:      storage.EpisodeClosed,
			FirstSellAt: 160, InitialBuyUSD: 100, AddBuyUSD: 50,
			SellLegs: 2, PartialExitLegs: 1, DataGap: false, OriginQuality: storage.OriginConfirmedZero},
		// closed, losing, no partial exit
		{Wallet: "W", Token: "T2", OpenedAt: 500, ClosedAt: 800, Adds: 0, Reduces: 1,
			CapitalIn: 200, CapitalOut: 100, RealizedPnL: -100, HoldDurationS: 300,
			Status:      storage.EpisodeClosed,
			FirstSellAt: 800, InitialBuyUSD: 200, AddBuyUSD: 0,
			SellLegs: 1, PartialExitLegs: 0, DataGap: false, OriginQuality: storage.OriginConfirmedZero},
		// data-gap: InitialBuyUSD=0 must NOT enter size medians
		{Wallet: "W", Token: "T3", OpenedAt: 900, ClosedAt: 960, Adds: 0, Reduces: 1,
			CapitalIn: 50, CapitalOut: 80, RealizedPnL: 30, HoldDurationS: 60,
			Status:      storage.EpisodePartial,
			FirstSellAt: 960, InitialBuyUSD: 0, AddBuyUSD: 0,
			SellLegs: 1, PartialExitLegs: 0, DataGap: true, OriginQuality: storage.OriginCensored},
		// open, re-entry on T1
		{Wallet: "W", Token: "T1", OpenedAt: 1000, ClosedAt: 0, Adds: 0, Reduces: 0,
			CapitalIn: 30, CapitalOut: 0, RealizedPnL: 0, HoldDurationS: 0,
			Status:      storage.EpisodeOpen,
			FirstSellAt: 0, InitialBuyUSD: 30, AddBuyUSD: 0,
			SellLegs: 0, PartialExitLegs: 0, DataGap: false, OriginQuality: storage.OriginConfirmedZero},
	}
	// entries: T1 initial (+chase, valid prior), T1 add (+chase, since 60s),
	// T2 initial (+chase, prior INVALID — window before dataset start),
	// T1 re-entry initial
	entries := []EntryFact{
		{Initial: true, TradeTime: 100, ReceivedAt: 130,
			OriginQuality: storage.OriginConfirmedZero,
			ChasePct:      1, HasChase: true, SmartPrior: 2, KOLPrior: 0, PriorValid: true},
		{Initial: false, TradeTime: 150, ReceivedAt: 190,
			ChasePct: 3, HasChase: true, SinceInitialSecs: 50, OriginQuality: storage.OriginConfirmedZero},
		{Initial: true, TradeTime: 500, ReceivedAt: 560,
			OriginQuality: storage.OriginConfirmedZero,
			ChasePct:      2, HasChase: true, SmartPrior: 9, KOLPrior: 1, PriorValid: false},
		{Initial: true, TradeTime: 1000, ReceivedAt: 1010,
			OriginQuality: storage.OriginConfirmedZero,
			ChasePct:      4, HasChase: true, SmartPrior: 4, KOLPrior: 1, PriorValid: true},
	}

	p := BuildProfile("W", episodes, entries)

	// ENTRY — initial vs add
	if p.Entry.InitialCount != 3 || p.Entry.InitialConfirmed != 3 ||
		p.Entry.InitialVisible != 0 || p.Entry.InitialCensored != 0 || p.Entry.AddCount != 1 {
		t.Errorf("initial = %d (conf %d, vis %d, cens %d) add %d, want 3 (3/0/0)/1",
			p.Entry.InitialCount, p.Entry.InitialConfirmed, p.Entry.InitialVisible, p.Entry.InitialCensored, p.Entry.AddCount)
	}
	if math.Abs(p.Entry.ReentryRate-0.25) > 0.001 { // 4 episodes, 3 tokens
		t.Errorf("reentry = %.2f, want 0.25", p.Entry.ReentryRate)
	}
	// median chase: INITIAL entries only → [1,2,4] (add's 3 excluded)
	if p.Entry.MedianChase.Value != 2 || p.Entry.MedianChase.N != 3 {
		t.Errorf("median chase = %.0f (n%d), want 2 (n3, initial only)", p.Entry.MedianChase.Value, p.Entry.MedianChase.N)
	}
	if p.Entry.MedianAddChase.Value != 3 || p.Entry.MedianAddChase.N != 1 {
		t.Errorf("median add chase = %.0f (n%d), want 3 (n1)", p.Entry.MedianAddChase.Value, p.Entry.MedianAddChase.N)
	}
	if p.Entry.MedianSinceInitialSecs.Value != 50 || p.Entry.MedianSinceInitialSecs.N != 1 {
		t.Errorf("since initial = %.0f (n%d), want 50 (n1)", p.Entry.MedianSinceInitialSecs.Value, p.Entry.MedianSinceInitialSecs.N)
	}
	// prior flow: only VALID windows → [2,4] (the T2 window predates the dataset)
	if p.Entry.SmartPriorP50.Value != 3 || p.Entry.SmartPriorP50.N != 2 || p.Entry.PriorFlowN != 2 {
		t.Errorf("prior smart = %.0f (n%d, flowN %d), want 3 (n2)", p.Entry.SmartPriorP50.Value, p.Entry.SmartPriorP50.N, p.Entry.PriorFlowN)
	}
	if math.Abs(p.Entry.Cluster3Plus-0.50) > 0.001 { // 4 of [2,4] >= 3
		t.Errorf("cluster3plus = %.2f, want 0.50", p.Entry.Cluster3Plus)
	}
	// sizes: gap episode's $0 must NOT enter — initial buys [100,200,30] → 100
	if p.Entry.MedianInitialBuy.Value != 100 || p.Entry.MedianInitialBuy.N != 3 {
		t.Errorf("initial buy = %.0f (n%d), want 100 (n3, gap excluded)", p.Entry.MedianInitialBuy.Value, p.Entry.MedianInitialBuy.N)
	}
	// add capital: only episodes that added → [50]
	if p.Entry.MedianAddBuy.Value != 50 || p.Entry.MedianAddBuy.N != 1 {
		t.Errorf("add buy = %.0f (n%d), want 50 (n1)", p.Entry.MedianAddBuy.Value, p.Entry.MedianAddBuy.N)
	}
	if math.Abs(p.Entry.AddEpisodeRate-0.25) > 0.001 {
		t.Errorf("add episode rate = %.2f, want 0.25", p.Entry.AddEpisodeRate)
	}

	// POSITION — hold median only over closed (180, 300)
	if p.Position.Episodes != 4 || p.Position.Trusted != 3 || p.Position.Censored != 1 {
		t.Errorf("episodes = %d (trusted %d censored %d), want 4 (3/1)",
			p.Position.Episodes, p.Position.Trusted, p.Position.Censored)
	}
	if p.Position.MedianHoldSecs.Value != 240 || p.Position.MedianHoldSecs.N != 2 {
		t.Errorf("median hold = %.0f (n%d), want 240 (n2, closed only)", p.Position.MedianHoldSecs.Value, p.Position.MedianHoldSecs.N)
	}

	// EXIT — real partial exits: only T1 (1 of 2 fully-visible observable)
	if math.Abs(p.Exit.PartialExitRatio-0.50) > 0.001 || p.Exit.PartialExitN != 1 || p.Exit.ObservableN != 2 {
		t.Errorf("partial exits = %.2f (%d of %d), want 0.50 (1 of 2)", p.Exit.PartialExitRatio, p.Exit.PartialExitN, p.Exit.ObservableN)
	}
	if p.Exit.FirstSellP50.Value != 180 || p.Exit.FirstSellP50.N != 2 { // 60, 300
		t.Errorf("first sell P50 = %.0f (n%d), want 180 (n2)", p.Exit.FirstSellP50.Value, p.Exit.FirstSellP50.N)
	}
	if p.Exit.CloseP50.Value != 240 || p.Exit.CloseP50.N != 2 {
		t.Errorf("close P50 = %.0f (n%d), want 240 (n2)", p.Exit.CloseP50.Value, p.Exit.CloseP50.N)
	}
	if p.Exit.ClosedPnl != -50 || math.Abs(p.Exit.ClosedWinRate-0.50) > 0.001 {
		t.Errorf("closed pnl/win = %.0f/%.2f, want -50/0.50", p.Exit.ClosedPnl, p.Exit.ClosedWinRate)
	}
	if p.Exit.CensoredPnl != 30 { // the data-gap (censored) episode's pnl
		t.Errorf("censored pnl = %.0f, want 30", p.Exit.CensoredPnl)
	}
	if math.Abs(p.Exit.IncompleteRatio-0.25) > 0.001 || p.Exit.IncompletePnl != 30 {
		t.Errorf("incomplete = %.2f pnl %.0f, want 0.25/30", p.Exit.IncompleteRatio, p.Exit.IncompletePnl)
	}

	// missing data → n/a (N=0), not a fabricated zero
	empty := BuildProfile("W", nil, nil)
	if empty.Entry.MedianChase.N != 0 || empty.Entry.MedianAge.N != 0 ||
		empty.Exit.FirstSellP50.N != 0 || empty.Exit.CloseP50.N != 0 {
		t.Errorf("empty profile must carry N=0, got %+v", empty)
	}
}

// TestBuildProfileWindowCohort pins the cohort semantics at the profile
// level. The storage layer already filters episodes to opened_at >= since
// (ReconstructEpisodesFor); this test simulates that output and verifies
// the profile consumes only the window cohort: the add inside the window
// counts as an add, and the pre-window opening buy's size survives because
// reconstruction ran over full history.
func TestBuildProfileWindowCohort(t *testing.T) {
	episodes := []storage.Episode{
		// opened 2h ago (before a 24h-window? no — before a 1h window), added
		// 30m ago, closed 10m ago. Reconstructed from FULL history, so the
		// initial buy ($100) is known even though it predates the window.
		{Wallet: "W", Token: "T1", OpenedAt: 0, ClosedAt: 0, Adds: 1, Reduces: 1,
			CapitalIn: 150, CapitalOut: 140, RealizedPnL: -10, HoldDurationS: 0,
			Status:      storage.EpisodeOpen,
			FirstSellAt: 0, InitialBuyUSD: 100, AddBuyUSD: 50,
			SellLegs: 0, PartialExitLegs: 0, DataGap: false, OriginQuality: storage.OriginConfirmedZero},
	}
	entries := []EntryFact{
		{Initial: false, TradeTime: 100, ReceivedAt: 130, SinceInitialSecs: 100}, // the add
	}
	p := BuildProfile("W", episodes, entries)
	if p.Entry.InitialCount != 0 || p.Entry.AddCount != 1 {
		t.Errorf("initial/add = %d/%d, want 0/1 (window shows only the add)", p.Entry.InitialCount, p.Entry.AddCount)
	}
	if p.Entry.MedianInitialBuy.Value != 100 || p.Entry.MedianInitialBuy.N != 1 {
		t.Errorf("initial buy = %.0f (n%d), want 100 (n1 — full-history reconstruction)", p.Entry.MedianInitialBuy.Value, p.Entry.MedianInitialBuy.N)
	}
}
