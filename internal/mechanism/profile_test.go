package mechanism

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/auren23/followedge/internal/domain"
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
			SellLegs: 0, PartialExitLegs: 0, DataGap: false, OriginQuality: storage.OriginConfirmedZero,
			IsReentry: true}, // T1 had an earlier episode — fixed at reconstruction
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
	if p.Entry.InitialCount != 3 || p.Entry.Initial.Confirmed != 3 ||
		p.Entry.Initial.Visible != 0 || p.Entry.Initial.Censored != 0 ||
		p.Entry.Initial.DataGap != 0 || p.Entry.AddCount != 1 {
		t.Errorf("initial = %d (conf %d, vis %d, cens %d, gap %d) add %d, want 3 (3/0/0/0)/1",
			p.Entry.InitialCount, p.Entry.Initial.Confirmed, p.Entry.Initial.Visible,
			p.Entry.Initial.Censored, p.Entry.Initial.DataGap, p.Entry.AddCount)
	}
	if math.Abs(p.Entry.ReentryRate.Strict.Value-(1.0/3.0)) > 0.001 { // 3 confirmed episodes, 2 tokens
		t.Errorf("reentry (strict) = %.2f, want 0.33", p.Entry.ReentryRate.Strict.Value)
	}
	// median chase: INITIAL entries only → [1,2,4] (add's 3 excluded)
	if p.Entry.MedianChase.Strict.Value != 2 || p.Entry.MedianChase.Strict.N != 3 {
		t.Errorf("median chase = %.0f (n%d), want 2 (n3, initial only)", p.Entry.MedianChase.Strict.Value, p.Entry.MedianChase.Strict.N)
	}
	if p.Entry.MedianAddChase.Strict.Value != 3 || p.Entry.MedianAddChase.Strict.N != 1 {
		t.Errorf("median add chase = %.0f (n%d), want 3 (n1)", p.Entry.MedianAddChase.Strict.Value, p.Entry.MedianAddChase.Strict.N)
	}
	if p.Entry.MedianSinceInitialSecs.Strict.Value != 50 || p.Entry.MedianSinceInitialSecs.Strict.N != 1 {
		t.Errorf("since initial = %.0f (n%d), want 50 (n1)", p.Entry.MedianSinceInitialSecs.Strict.Value, p.Entry.MedianSinceInitialSecs.Strict.N)
	}
	// prior flow: only VALID windows → [2,4] (the T2 window predates the dataset)
	if p.Entry.SmartPriorP50.Strict.Value != 3 || p.Entry.SmartPriorP50.Strict.N != 2 {
		t.Errorf("prior smart = %.0f (n%d, flowN %d), want 3 (n2)", p.Entry.SmartPriorP50.Strict.Value, p.Entry.SmartPriorP50.Strict.N, p.Entry.SmartPriorP50.Strict.N)
	}
	if math.Abs(p.Entry.Cluster3Plus.Strict.Value-0.50) > 0.001 { // 4 of [2,4] >= 3
		t.Errorf("cluster3plus = %.2f, want 0.50", p.Entry.Cluster3Plus.Strict.Value)
	}
	// sizes: gap episode's $0 must NOT enter — initial buys [100,200,30] → 100
	if p.Entry.MedianInitialBuy.Strict.Value != 100 || p.Entry.MedianInitialBuy.Strict.N != 3 {
		t.Errorf("initial buy = %.0f (n%d), want 100 (n3, gap excluded)", p.Entry.MedianInitialBuy.Strict.Value, p.Entry.MedianInitialBuy.Strict.N)
	}
	// add capital: only episodes that added → [50]
	if p.Entry.MedianAddBuy.Strict.Value != 50 || p.Entry.MedianAddBuy.Strict.N != 1 {
		t.Errorf("add buy = %.0f (n%d), want 50 (n1)", p.Entry.MedianAddBuy.Strict.Value, p.Entry.MedianAddBuy.Strict.N)
	}
	if math.Abs(p.Entry.AddEpisodeRate.Strict.Value-(1.0/3.0)) > 0.001 { // 1 of 3 confirmed episodes added
		t.Errorf("add episode rate (strict) = %.2f, want 0.33", p.Entry.AddEpisodeRate.Strict.Value)
	}

	// POSITION — hold median only over closed (180, 300)
	if p.Position.Episodes != 4 ||
		p.Position.Evidence.Confirmed != 3 || p.Position.Evidence.Visible != 0 ||
		p.Position.Evidence.Censored != 0 || p.Position.Evidence.DataGap != 1 {
		t.Errorf("episodes = %d evidence %+v, want 4 (conf 3 / vis 0 / cens 0 / gap 1)",
			p.Position.Episodes, p.Position.Evidence)
	}
	if p.Position.MedianHoldSecs.Strict.Value != 240 || p.Position.MedianHoldSecs.Strict.N != 2 {
		t.Errorf("median hold = %.0f (n%d), want 240 (n2, closed only)", p.Position.MedianHoldSecs.Strict.Value, p.Position.MedianHoldSecs.Strict.N)
	}

	// EXIT — real partial exits: only T1 (1 of 2 fully-visible confirmed)
	if math.Abs(p.Exit.PartialExitRatio.Strict.Value-0.50) > 0.001 ||
		p.Exit.PartialExitRatio.Strict.N != 2 ||
		math.Abs(p.Exit.PartialExitRatio.Strict.Value*float64(p.Exit.PartialExitRatio.Strict.N)-1) > 0.001 {
		t.Errorf("partial exits = %.2f (n=%d), want 0.50 (1 of 2)", p.Exit.PartialExitRatio.Strict.Value, p.Exit.PartialExitRatio.Strict.N)
	}
	if p.Exit.FirstSellP50.Strict.Value != 180 || p.Exit.FirstSellP50.Strict.N != 2 { // 60, 300
		t.Errorf("first sell P50 = %.0f (n%d), want 180 (n2)", p.Exit.FirstSellP50.Strict.Value, p.Exit.FirstSellP50.Strict.N)
	}
	if p.Exit.CloseP50.Strict.Value != 240 || p.Exit.CloseP50.Strict.N != 2 {
		t.Errorf("close P50 = %.0f (n%d), want 240 (n2)", p.Exit.CloseP50.Strict.Value, p.Exit.CloseP50.Strict.N)
	}
	if p.Exit.ClosedPnl.Strict.Value != -50 || math.Abs(p.Exit.ClosedWinRate.Strict.Value-0.50) > 0.001 {
		t.Errorf("closed pnl/win = %.0f/%.2f, want -50/0.50", p.Exit.ClosedPnl.Strict.Value, p.Exit.ClosedWinRate.Strict.Value)
	}
	// pnl buckets are MUTUALLY EXCLUSIVE: the gap episode's pnl lives in
	// IncompletePnl, censored-complete stays 0
	if p.Exit.CensoredPnl != 0 || p.Exit.IncompletePnl != 30 {
		t.Errorf("pnl buckets = censored %.0f incomplete %.0f, want 0/30", p.Exit.CensoredPnl, p.Exit.IncompletePnl)
	}
	if math.Abs(p.Exit.IncompleteRatio-0.25) > 0.001 || p.Exit.IncompletePnl != 30 {
		t.Errorf("incomplete = %.2f pnl %.0f, want 0.25/30", p.Exit.IncompleteRatio, p.Exit.IncompletePnl)
	}

	// missing data → n/a (N=0), not a fabricated zero
	empty := BuildProfile("W", nil, nil)
	if empty.Entry.MedianChase.Strict.N != 0 || empty.Entry.MedianAge.Strict.N != 0 ||
		empty.Exit.FirstSellP50.Strict.N != 0 || empty.Exit.CloseP50.Strict.N != 0 ||
		empty.Entry.MedianChase.Research.N != 0 {
		t.Errorf("empty profile must carry N=0 on both channels, got %+v", empty)
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
	if p.Entry.MedianInitialBuy.Strict.Value != 100 || p.Entry.MedianInitialBuy.Strict.N != 1 {
		t.Errorf("initial buy = %.0f (n%d), want 100 (n1 — full-history reconstruction)", p.Entry.MedianInitialBuy.Strict.Value, p.Entry.MedianInitialBuy.Strict.N)
	}
}

// TestProductionPathNeverConfirms is the end-to-end evidence trap: the REAL
// storage reconstruction has no way to produce OriginConfirmedZero (no
// source proves a zero balance), so a profile built from production-shaped
// data must show confirmed=0 everywhere while the research channel carries
// the post-visible-close episodes.
func TestProductionPathNeverConfirms(t *testing.T) {
	s, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	base := now.Add(-2 * time.Hour).Truncate(30 * time.Second)
	mk := func(id, token, side string, ts time.Time, qty float64) {
		ev := domain.TradeEvent{
			ID:     domain.EventID("sol", id, "W_E2E", token, side),
			Source: "gmgn_smartmoney", Chain: "sol", TxHash: id,
			Wallet: "W_E2E", WalletType: domain.WalletSmartMoney,
			TokenAddress: token, Side: domain.Side(side), AmountUSD: qty,
			TokenAmount: qty, PriceUSD: 1.0, BuyCostUSD: 0,
			TradeTime: ts, ReceivedAt: ts,
		}
		if _, err := s.InsertEvent(ev); err != nil {
			t.Fatal(err)
		}
	}
	mk("e1", "T1", "buy", base, 100)
	mk("e2", "T1", "sell", base.Add(30*time.Second), 100) // visible zero
	mk("e3", "T1", "buy", base.Add(60*time.Second), 50)   // VisibleZero episode
	mk("e4", "T1", "sell", base.Add(90*time.Second), 50)  // closes

	episodes, err := s.ReconstructEpisodesFor("W_E2E", now.Add(-3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	classified, err := s.ClassifiedEntries("W_E2E")
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]EntryFact, 0, len(classified))
	for _, ce := range classified {
		entries = append(entries, EntryFact{
			Initial: ce.Initial, TradeTime: ce.TradeTime, ReceivedAt: ce.ReceivedAt,
			SinceInitialSecs: ce.SinceInitialSecs, OriginQuality: ce.OriginQuality,
			DataGap: ce.DataGap,
		})
	}
	p := BuildProfile("W_E2E", episodes, entries)

	// e1 is the first observed episode (Censored), e3 sits after a visible
	// close (VisibleZero) — nothing can be Confirmed on the production path
	if p.Entry.Initial.Confirmed != 0 || p.Entry.Initial.Visible != 1 ||
		p.Entry.Initial.Censored != 1 || p.Entry.Initial.DataGap != 0 {
		t.Errorf("initial split = %+v, want conf 0 / vis 1 / cens 1 / gap 0", p.Entry.Initial)
	}
	if p.Position.Evidence.StrictN() != 0 || p.Position.Evidence.ResearchN() != 1 ||
		p.Position.Evidence.Censored != 1 {
		t.Errorf("episodes evidence = %+v, want strict 0 / research 1 / censored 1 (visible is NOT censored)",
			p.Position.Evidence)
	}
	// strict channel must be completely empty; research channel has data
	if p.Entry.MedianChase.Strict.N != 0 || p.Entry.MedianInitialBuy.Strict.N != 0 {
		t.Errorf("strict channel must be empty, got %+v", p.Entry)
	}
	if p.Position.MedianHoldSecs.Research.N == 0 {
		t.Errorf("research channel must carry the visible-zero episodes, got %+v", p.Position.MedianHoldSecs)
	}
	// e3 re-enters T1 (its first episode was censored but IS an episode) —
	// the research reentry rate must be 100%, not 0: evidence policy selects
	// which episodes enter the denominator, it never redefines who re-enters.
	if p.Entry.ReentryRate.Strict.N != 0 || p.Entry.ReentryRate.Research.N != 1 ||
		math.Abs(p.Entry.ReentryRate.Research.Value-1.0) > 0.001 {
		t.Errorf("reentry = strict n%d research n%d v%.2f, want 0/1/1.00 (censored first episode still marks a re-entry)",
			p.Entry.ReentryRate.Strict.N, p.Entry.ReentryRate.Research.N, p.Entry.ReentryRate.Research.Value)
	}
}
