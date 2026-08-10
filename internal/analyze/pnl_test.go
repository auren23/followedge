package analyze

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/auren23/followedge/internal/domain"
	"github.com/auren23/followedge/internal/storage"
)

// P0-4: validate the GMGN-derived realized PnL formula under partial exits.
//
// RealizedPnL = Σ over sells of (amount_usd − buy_cost_usd), where
// buy_cost_usd is what GMGN reports as the sell leg's original cost basis.
// We cannot verify GMGN's basis attribution without chain-level reconstruction
// (v0.2 work), but we CAN pin down the formula's behavior on the data shapes
// GMGN actually emits, so its semantics are explicit and regression-proof.
func TestRealizedPnLUnderPartialExits(t *testing.T) {
	s, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	w := "W_PNL"
	token := "TOKEN_PNL"
	seq := 0
	mk := func(side domain.Side, amountUSD, buyCostUSD float64, tokenDay string) domain.TradeEvent {
		seq++
		ts := now.Add(-time.Duration(seq) * time.Minute)
		return domain.TradeEvent{
			ID:           domain.EventID("sol", string(rune('a'+seq)), w, token, string(side)),
			Source:       "gmgn_smartmoney",
			Chain:        "sol",
			TxHash:       string(rune('a' + seq)),
			Wallet:       w,
			WalletType:   domain.WalletSmartMoney,
			TokenAddress: token,
			Side:         side,
			AmountUSD:    amountUSD,
			PriceUSD:     1.0,
			BuyCostUSD:   buyCostUSD,
			TradeTime:    ts,
			ReceivedAt:   ts,
		}
	}

	// scenario 1: full exit — buy $100, sell $120 with cost $100 → +$20
	insert(t, s, mk(domain.Buy, 100, 0, "d1"))
	insert(t, s, mk(domain.Sell, 120, 100, "d1"))

	// scenario 2: partial exits (GMGN reports a pro-rata cost basis per leg) —
	// two sells of $60 each with cost $50 each → +$20 total, same as full exit
	insert(t, s, mk(domain.Sell, 60, 50, "d1"))
	insert(t, s, mk(domain.Sell, 60, 50, "d1"))

	// scenario 3: sell with missing cost basis (buy_cost_usd = 0) must NOT be
	// counted as a $60 profit — it is skipped, not misattributed
	insert(t, s, mk(domain.Sell, 60, 0, "d1"))

	groups, err := s.ActorGroups(now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) == 0 {
		t.Fatal("no groups")
	}
	var realized float64
	for _, g := range groups {
		realized += g.RealizedPnL
	}
	// expected: 20 + (10 + 10) = 40; the cost-less sell contributes 0
	if realized != 40 {
		t.Fatalf("realized pnl = %v, want 40 (full-exit 20 + partials 20; missing-cost sell skipped)", realized)
	}
}

func insert(t *testing.T, s *storage.Store, ev domain.TradeEvent) {
	t.Helper()
	created, err := s.InsertEvent(ev)
	if err != nil || !created {
		t.Fatalf("insert: created=%v err=%v", created, err)
	}
}

// TestChaseBucketsByChaseNotReturn pins the P0-2 fix: buckets are chosen by
// entry chase, statistics are computed on follower returns. Before the fix
// the code bucketed by ReturnPct itself — a tautology.
func TestChaseBucketsByChaseNotReturn(t *testing.T) {
	s, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	// three events, same token, different chase/return combos:
	//   E1: chase 1%  → follower return +5%
	//   E2: chase 1%  → follower return −3%
	//   E3: chase 25% → follower return +8%
	cases := []struct {
		id    string
		chase float64 // entry price / leader price − 1
		ret   float64 // follower return
	}{
		{"e1", 0.01, 5},
		{"e2", 0.01, -3},
		{"e3", 0.25, 8},
	}
	for _, c := range cases {
		ts := now.Add(-10 * time.Minute)
		ev := domain.TradeEvent{
			ID:     domain.EventID("sol", c.id, "W_C", "TOKEN_C", "buy"),
			Source: "gmgn_smartmoney", Chain: "sol", TxHash: c.id,
			Wallet: "W_C", WalletType: domain.WalletSmartMoney,
			TokenAddress: "TOKEN_C", Side: domain.Buy, AmountUSD: 10, PriceUSD: 1.0,
			TradeTime: ts, ReceivedAt: ts,
		}
		insert(t, s, ev)
		// follower row: entry price = leader*(1+chase), observed = entry*(1+ret/100)
		entry := 1.0 * (1 + c.chase)
		observed := entry * (1 + c.ret/100)
		if err := s.CreateMarkouts(ev, storage.MarkoutFollower, &entry, []time.Duration{30 * time.Second}, now); err != nil {
			t.Fatal(err)
		}
		if err := s.FillMarkout(ev.ID, storage.MarkoutFollower, 30*time.Second, observed, 0); err != nil {
			t.Fatal(err)
		}
	}

	var buf sink
	if err := Chase(&buf, s, 30*time.Second, "buy"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	line := func(prefix string) string {
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, prefix) {
				return l
			}
		}
		return ""
	}
	// "0-2%" bucket must contain E1(+5) and E2(−3): N=2, WR 50%, avg +1.0
	l := line("0-2%")
	if !strings.Contains(l, "2") || !strings.Contains(l, "50.0%") || !strings.Contains(l, "+1.00%") {
		t.Errorf("0-2%% bucket wrong (should be bucketed by chase, stats on follower returns):\n%s", out)
	}
	// "20%+" bucket must contain E3(+8): N=1, avg +8 — bucketed by chase, not return
	l = line("20%+")
	if !strings.Contains(l, "1") || !strings.Contains(l, "100.0%") || !strings.Contains(l, "+8.00%") {
		t.Errorf("20%%+ bucket wrong:\n%s", out)
	}
}

type sink struct{ s string }

func (w *sink) Write(p []byte) (int, error) { w.s += string(p); return len(p), nil }
func (w *sink) String() string              { return w.s }
