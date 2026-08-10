package analyze

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/auren23/followedge/internal/domain"
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
	actors := actorRows(groups)

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
	// replicabilityScore is gone: the replication census shows facts
	// (coverage, obs EV, cons EV, loss decomposition) instead of a single
	// 0-100 score that a filled-only mean could flatter.
}

// mkReplEvent inserts one trade + one follower markout at 30s horizon.
func mkReplEvent(t *testing.T, s *storage.Store, wallet, id string, ts time.Time, pnl float64, ret *float64, status string) {
	side := domain.Buy
	amount := 100.0
	buyCost := 0.0
	if pnl > 0 { // a sell leg that realizes pnl
		side = domain.Sell
		amount = pnl + 100
		buyCost = 100
	}
	ev := domain.TradeEvent{
		ID:     domain.EventID("sol", id, wallet, "T_"+id, string(side)),
		Source: "gmgn_smartmoney", Chain: "sol", TxHash: id,
		Wallet: wallet, WalletType: domain.WalletSmartMoney,
		TokenAddress: "T_" + id, Side: side, AmountUSD: amount, BuyCostUSD: buyCost, PriceUSD: 1.0,
		TradeTime: ts, ReceivedAt: ts,
	}
	if _, err := s.InsertEvent(ev); err != nil {
		t.Fatal(err)
	}
	entry := 1.0
	if err := s.CreateMarkouts(ev, storage.MarkoutFollower, &entry, []time.Duration{30 * time.Second}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	switch {
	case ret != nil:
		if err := s.FillMarkout(ev.ID, storage.MarkoutFollower, 30*time.Second, 1.0+*ret/100, 0); err != nil {
			t.Fatal(err)
		}
	case status != "":
		if err := s.SetMarkoutStatus(ev.ID, storage.MarkoutFollower, 30*time.Second, status); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRankCoverageAware pins the P0 fix: the actor table reports the
// coverage-aware census, so a wallet whose tokens mostly died shows low
// coverage and a conservative EV dragged down by market loss — no more
// filled-only mean flattery.
func TestRankCoverageAware(t *testing.T) {
	s, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	ts := now.Add(-20 * time.Minute)
	p5, p50 := 5.0, 50.0
	// W_A: 3 filled +5% → coverage 100%, cons EV +5
	for i := 0; i < 3; i++ {
		mkReplEvent(t, s, "W_A", fmt.Sprintf("a%d", i), ts, 0, &p5, "")
	}
	// W_B: 1 filled +50%, 4 no_candle → coverage 20%, cons EV (50-400)/5 = -70
	for i := 0; i < 4; i++ {
		mkReplEvent(t, s, "W_B", fmt.Sprintf("b%d", i), ts, 0, nil, storage.MarkoutStatusNoCandle)
	}
	mkReplEvent(t, s, "W_B", "b4", ts, 0, &p50, "")

	var buf sink
	if err := Rank(&buf, s, now.Add(-24*time.Hour), 30*time.Second, 10, 2, 100, 0, SortQuality, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	t.Log(out)

	if !strings.Contains(out, "100.0%") || !strings.Contains(out, "+5.00%") {
		t.Errorf("W_A coverage/consEV missing (want 100.0%% / +5.00%%): %s", out)
	}
	if !strings.Contains(out, "20.0%") || !strings.Contains(out, "-70.00%") {
		t.Errorf("W_B market loss must drag consEV (want 20.0%% coverage / -70.00%%): %s", out)
	}
	if !strings.Contains(out, "+50.00%") {
		t.Errorf("W_B observed EV must still show the +50%% fill: %s", out)
	}
	if strings.Contains(out, "repl") && !strings.Contains(out, "consEV") {
		t.Errorf("old single repl score must be gone, census columns expected: %s", out)
	}
}

// TestRankSortAndFrontier pins --sort and --frontier: sorting by different
// axes reorders the same facts, and the Pareto frontier drops actors
// strictly dominated on both (quality, conservative EV).
func TestRankSortAndFrontier(t *testing.T) {
	s, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	ts := now.Add(-20 * time.Minute)
	p5, p50 := 5.0, 50.0
	// A: big PnL, terrible replication (cons -85); C: same replication,
	//    lower PnL → dominated by A; B: small PnL, perfect replication.
	for i := 0; i < 9; i++ {
		mkReplEvent(t, s, "W_A", fmt.Sprintf("a%d", i), ts, 0, nil, storage.MarkoutStatusNoCandle)
	}
	mkReplEvent(t, s, "W_A", "a9", ts, 10000, &p50, "")
	for i := 0; i < 10; i++ {
		mkReplEvent(t, s, "W_B", fmt.Sprintf("b%d", i), ts, 0, &p5, "")
	}
	mkReplEvent(t, s, "W_B", "b10", ts, 100, &p5, "")
	for i := 0; i < 9; i++ {
		mkReplEvent(t, s, "W_C", fmt.Sprintf("c%d", i), ts, 0, nil, storage.MarkoutStatusNoCandle)
	}
	mkReplEvent(t, s, "W_C", "c9", ts, 5000, &p50, "")

	rank := func(key ActorSortKey, frontier bool) string {
		var buf sink
		if err := Rank(&buf, s, now.Add(-24*time.Hour), 30*time.Second, 10, 2, 100, 0, key, frontier); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	idx := func(out, w string) int { return strings.Index(out, w+" ") }

	// --sort pnl: A (10000) > C (5000) > B (100)
	out := rank(SortPnl, false)
	if !(idx(out, "W_A") < idx(out, "W_C") && idx(out, "W_C") < idx(out, "W_B")) {
		t.Errorf("--sort pnl order wrong:\n%s", out)
	}
	// --sort replicability (consEV desc): B (+5) > A/C (-85, stable → A first)
	out = rank(SortReplicability, false)
	if !(idx(out, "W_B") < idx(out, "W_A") && idx(out, "W_A") < idx(out, "W_C")) {
		t.Errorf("--sort replicability order wrong:\n%s", out)
	}
	// --frontier: C is strictly dominated by A (same consEV, lower quality)
	out = rank(SortQuality, true)
	if strings.Contains(out, "W_C") {
		t.Errorf("--frontier must drop dominated actor W_C:\n%s", out)
	}
	if !strings.Contains(out, "W_A") || !strings.Contains(out, "W_B") {
		t.Errorf("--frontier must keep W_A and W_B:\n%s", out)
	}
}
