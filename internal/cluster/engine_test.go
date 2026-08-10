package cluster

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/auren23/followedge/internal/domain"
	"github.com/auren23/followedge/internal/storage"
)

func testStore(t *testing.T) *storage.Store {
	t.Helper()
	s, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func insert(t *testing.T, s *storage.Store, ev domain.TradeEvent) {
	t.Helper()
	created, err := s.InsertEvent(ev)
	if err != nil || !created {
		t.Fatalf("insert: created=%v err=%v", created, err)
	}
}

func TestEngineDistinctWallets(t *testing.T) {
	s := testStore(t)
	eng := NewEngine(s, []time.Duration{30 * time.Second, 60 * time.Second})

	token := "TOKEN_A"
	now := time.Now().UTC()
	// 3 distinct smart wallets buy within 30s
	for i, w := range []string{"W1", "W2", "W3"} {
		insert(t, s, domain.TradeEvent{
			ID:     domain.EventID("sol", string(rune('a'+i)), w, token, "buy"),
			Source: "gmgn_smartmoney", Chain: "sol", TxHash: string(rune('a' + i)),
			Wallet: w, WalletType: domain.WalletSmartMoney,
			TokenAddress: token, Side: domain.Buy, AmountUSD: 100,
			TradeTime: now.Add(-time.Duration(i) * 5 * time.Second), ReceivedAt: now,
		})
	}
	// same wallet buys again (should not add a 4th distinct wallet)
	insert(t, s, domain.TradeEvent{
		ID:     domain.EventID("sol", "x", "W1", token, "buy"),
		Source: "gmgn_smartmoney", Chain: "sol", TxHash: "x",
		Wallet: "W1", WalletType: domain.WalletSmartMoney,
		TokenAddress: token, Side: domain.Buy, AmountUSD: 500,
		TradeTime: now.Add(-2 * time.Second), ReceivedAt: now,
	})
	// a KOL buys
	insert(t, s, domain.TradeEvent{
		ID:     domain.EventID("sol", "k", "K1", token, "buy"),
		Source: "gmgn_kol", Chain: "sol", TxHash: "k",
		Wallet: "K1", WalletType: domain.WalletKOL,
		TokenAddress: token, Side: domain.Buy, AmountUSD: 50,
		TradeTime: now.Add(-1 * time.Second), ReceivedAt: now,
	})

	// trigger recompute with the newest event
	ev := domain.TradeEvent{
		ID:     domain.EventID("sol", "z", "W3", token, "buy"),
		Source: "gmgn_smartmoney", Chain: "sol", TxHash: "z",
		Wallet: "W3", WalletType: domain.WalletSmartMoney,
		TokenAddress: token, Side: domain.Buy, AmountUSD: 100,
		TradeTime: now, ReceivedAt: now,
	}
	insert(t, s, ev)
	if err := eng.OnEvent(context.Background(), ev); err != nil {
		t.Fatal(err)
	}

	samples, err := s.ClusterSamples(30*time.Second, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) == 0 {
		t.Fatal("no samples written")
	}
	last := samples[0] // ts desc
	if last.SmartBuyWallets != 3 {
		t.Errorf("smart buy wallets = %d, want 3 (distinct!)", last.SmartBuyWallets)
	}
	if last.KOLBuyWallets != 1 {
		t.Errorf("kol buy wallets = %d, want 1", last.KOLBuyWallets)
	}
	if last.EventCount != 6 {
		t.Errorf("event count = %d, want 6 (trigger event included)", last.EventCount)
	}
}

func TestEngineWindowCutoff(t *testing.T) {
	s := testStore(t)
	eng := NewEngine(s, []time.Duration{30 * time.Second})
	token := "TOKEN_B"
	now := time.Now().UTC()

	// old event outside the 30s window
	insert(t, s, domain.TradeEvent{
		ID:     domain.EventID("sol", "old", "W1", token, "buy"),
		Source: "gmgn_smartmoney", Chain: "sol", TxHash: "old",
		Wallet: "W1", WalletType: domain.WalletSmartMoney,
		TokenAddress: token, Side: domain.Buy, AmountUSD: 100,
		TradeTime: now.Add(-2 * time.Minute), ReceivedAt: now,
	})
	// fresh event inside the window
	ev := domain.TradeEvent{
		ID:     domain.EventID("sol", "new", "W2", token, "buy"),
		Source: "gmgn_smartmoney", Chain: "sol", TxHash: "new",
		Wallet: "W2", WalletType: domain.WalletSmartMoney,
		TokenAddress: token, Side: domain.Buy, AmountUSD: 100,
		TradeTime: now, ReceivedAt: now,
	}
	insert(t, s, ev)
	if err := eng.OnEvent(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	samples, err := s.ClusterSamples(30*time.Second, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if samples[0].SmartBuyWallets != 1 || samples[0].EventCount != 1 {
		t.Errorf("window should exclude the 2m-old event: wallets=%d count=%d",
			samples[0].SmartBuyWallets, samples[0].EventCount)
	}
}
