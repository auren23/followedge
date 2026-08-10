package storage

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/auren23/followedge/internal/domain"
)

func TestMarkoutStatusLifecycle(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	ev := domain.TradeEvent{
		ID:     domain.EventID("sol", "tx1", "W1", "T1", "buy"),
		Source: "gmgn_smartmoney", Chain: "sol", TxHash: "tx1",
		Wallet: "W1", WalletType: domain.WalletSmartMoney,
		TokenAddress: "T1", Side: domain.Buy, AmountUSD: 10, PriceUSD: 1.0,
		TradeTime: now.Add(-10 * time.Minute), ReceivedAt: now.Add(-10 * time.Minute),
	}
	created, err := s.InsertEvent(ev)
	if err != nil || !created {
		t.Fatal("insert failed")
	}
	price := ev.PriceUSD
	if err := s.CreateMarkouts(ev, MarkoutLeader, &price, []time.Duration{30 * time.Second}, now); err != nil {
		t.Fatal(err)
	}

	// pending by default
	counts, pending, err := s.MarkoutStatusCounts(MarkoutLeader, 30*time.Second, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if counts[MarkoutStatusPending] != 1 {
		t.Errorf("expected 1 pending row, got %v", counts)
	}

	// mark no_candle → counted
	if err := s.SetMarkoutStatus(ev.ID, MarkoutLeader, 30*time.Second, MarkoutStatusNoCandle); err != nil {
		t.Fatal(err)
	}
	// second attempt must NOT overwrite (first attempt wins)
	if err := s.SetMarkoutStatus(ev.ID, MarkoutLeader, 30*time.Second, MarkoutStatusAPIError); err != nil {
		t.Fatal(err)
	}
	counts, pending, err = s.MarkoutStatusCounts(MarkoutLeader, 30*time.Second, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if counts[MarkoutStatusNoCandle] != 1 || counts[MarkoutStatusAPIError] != 0 {
		t.Errorf("status should stay no_candle: %v", counts)
	}

	// fill wins over any status
	if err := s.FillMarkout(ev.ID, MarkoutLeader, 30*time.Second, 1.2); err != nil {
		t.Fatal(err)
	}
	counts, _, err = s.MarkoutStatusCounts(MarkoutLeader, 30*time.Second, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if counts[MarkoutStatusFilled] != 1 || counts[MarkoutStatusNoCandle] != 0 {
		t.Errorf("fill should supersede: %v", counts)
	}
	_ = pending
}

func TestRebuildEpisodes(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	// wallet W: BUY 500 → BUY 300 (add) → SELL 200 → SELL 900 → closed
	// wallet X: BUY 100 → SELL 50 → SELL 200 (partial: exceeds position)
	mk := func(id, wallet string, side domain.Side, tok string, buyCost float64, ts time.Time) {
		e := domain.TradeEvent{
			ID:     domain.EventID("sol", id, wallet, tok, string(side)),
			Source: "gmgn_smartmoney", Chain: "sol", TxHash: id,
			Wallet: wallet, WalletType: domain.WalletSmartMoney,
			TokenAddress: tok, Side: side, AmountUSD: 100,
			TokenAmount: 100, PriceUSD: 1.0, BuyCostUSD: buyCost,
			TradeTime: ts, ReceivedAt: ts,
		}
		created, err := s.InsertEvent(e)
		if err != nil || !created {
			t.Fatal("insert failed")
		}
	}
	base := now.Add(-time.Hour)
	mk("b1", "W", domain.Buy, "T", 0, base)
	mk("b2", "W", domain.Buy, "T", 0, base.Add(time.Minute))
	mk("s1", "W", domain.Sell, "T", 150, base.Add(2*time.Minute)) // cost basis 150
	mk("s2", "W", domain.Sell, "T", 150, base.Add(3*time.Minute)) // cost basis 150
	mk("b3", "X", domain.Buy, "U", 0, base)
	mk("s3", "X", domain.Sell, "U", 60, base.Add(time.Minute))
	mk("s4", "X", domain.Sell, "U", 60, base.Add(2*time.Minute)) // exceeds visible qty 100→40-100<0

	n, err := s.RebuildEpisodes(now.Add(-2 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// W: buy 500+300 → sell 200+900 (closed)
	// X: buy 100 → sell 100 (closed); stray sell 100 on empty position (partial)
	if n != 3 {
		t.Fatalf("expected 3 episodes, got %d", n)
	}

	// W: closed, 1 add, 2 reduces, pnl = (100-150)+(100-150) = -100 (sells
	// amount 100 each vs cost 150)
	var wClosed, xPartial, xClosed int
	var pnlW float64
	rows, err := s.db.Query(`SELECT wallet, status, adds, reduces, realized_pnl FROM position_episodes`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var wallet, status string
		var adds, reduces int
		var pnl float64
		if err := rows.Scan(&wallet, &status, &adds, &reduces, &pnl); err != nil {
			t.Fatal(err)
		}
		switch wallet {
		case "W":
			if status != "closed" || adds != 1 || reduces != 2 {
				t.Errorf("W episode wrong: %s adds=%d reduces=%d", status, adds, reduces)
			}
			pnlW = pnl
			wClosed++
		case "X":
			if status == "partial" { // stray sell on empty position
				xPartial++
				if pnl != 40 { // 100-60
					t.Errorf("X partial pnl = %v, want 40", pnl)
				}
			} else if status == "closed" {
				xClosed++
				if pnl != 40 { // 100-60
					t.Errorf("X closed pnl = %v, want 40", pnl)
				}
			}
		}
	}
	if wClosed != 1 || xPartial != 1 || xClosed != 1 {
		t.Errorf("episode statuses wrong: W closed=%d X partial=%d X closed=%d", wClosed, xPartial, xClosed)
	}
	if pnlW != -100 {
		t.Errorf("W pnl = %v, want -100", pnlW)
	}
}
