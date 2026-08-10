package analyze

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/auren23/followedge/internal/domain"
	"github.com/auren23/followedge/internal/storage"
)

// TestLatencyEVCoverageAndConservative pins the coverage-aware EV: unpriced
// due rows must enter the denominator and drag conservative EV down.
func TestLatencyEVCoverageAndConservative(t *testing.T) {
	s, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	// 4 follower rows at 30s horizon:
	//   E1, E2: filled (+10%, +30%) — source age 10s
	//   E3: due but never priced (no_candle) — source age 10s
	//   E4: filled (+5%) — source age 90s
	mk := func(id string, age time.Duration, ret *float64) {
		ts := now.Add(-20 * time.Minute)
		ev := domain.TradeEvent{
			ID:     domain.EventID("sol", id, "W", "T_"+id, "buy"),
			Source: "gmgn_smartmoney", Chain: "sol", TxHash: id,
			Wallet: "W", WalletType: domain.WalletSmartMoney,
			TokenAddress: "T_" + id, Side: domain.Buy, AmountUSD: 10, PriceUSD: 1.0,
			TradeTime: ts.Add(-age), ReceivedAt: ts,
		}
		insert(t, s, ev)
		entry := 1.0
		if err := s.CreateMarkouts(ev, storage.MarkoutFollower, &entry, []time.Duration{30 * time.Second}, now); err != nil {
			t.Fatal(err)
		}
		if ret != nil {
			if err := s.FillMarkout(ev.ID, storage.MarkoutFollower, 30*time.Second, 1.0+*ret/100); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := s.SetMarkoutStatus(ev.ID, storage.MarkoutFollower, 30*time.Second, storage.MarkoutStatusNoCandle); err != nil {
				t.Fatal(err)
			}
		}
	}
	p10 := 10.0
	p30 := 30.0
	p5 := 5.0
	mk("e1", 10*time.Second, &p10)
	mk("e2", 10*time.Second, &p30)
	mk("e3", 10*time.Second, nil) // unpriced
	mk("e4", 90*time.Second, &p5)

	var buf sink
	if err := LatencyEV(&buf, s, 30*time.Second, "", false, 100); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	t.Log(out)

	// 0-30s bucket: due=3, filled=2, cover 66.7%, obs EV = (10+30)/2 = +20,
	// cons EV = (40 - 100)/3 = -20
	if !strings.Contains(out, "0-30s") || !strings.Contains(out, "66.7%") {
		t.Errorf("0-30s bucket missing coverage: %s", out)
	}
	if !strings.Contains(out, "+20.00%") {
		t.Errorf("0-30s obs EV wrong: %s", out)
	}
	if !strings.Contains(out, "-20.00%") {
		t.Errorf("0-30s cons EV wrong (want -20.00 with 100%% loss on unpriced): %s", out)
	}
	// 60-120s bucket: due=1 filled=1 cover 100%, obs=cons=+5
	if !strings.Contains(out, "60-120s") || !strings.Contains(out, "100.0%") {
		t.Errorf("60-120s bucket missing: %s", out)
	}
}
