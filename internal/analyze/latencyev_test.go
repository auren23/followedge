package analyze

import (
	"path/filepath"
	"regexp"
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
			if err := s.FillMarkout(ev.ID, storage.MarkoutFollower, 30*time.Second, 1.0+*ret/100, 0); err != nil {
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
	if err := LatencyEV(&buf, s, 30*time.Second, "", false, 100, 0); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	t.Log(out)

	// 0-30s bucket: due=3, filled=2, cover 66.7%, obs EV = (10+30)/2 = +20,
	// cons EV = (40 - 100)/3 = -20 (no_candle is a MARKET outcome)
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

// TestLatencyEVSideFilter pins that --side is a REAL filter: sell rows must
// not dilute a buy-only bucket's coverage or EV.
func TestLatencyEVSideFilter(t *testing.T) {
	s, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	mk := func(id string, side domain.Side, ret float64) {
		ts := now.Add(-20 * time.Minute)
		ev := domain.TradeEvent{
			ID:     domain.EventID("sol", id, "W", "T_"+id, "buy"),
			Source: "gmgn_smartmoney", Chain: "sol", TxHash: id,
			Wallet: "W", WalletType: domain.WalletSmartMoney,
			TokenAddress: "T_" + id, Side: side, AmountUSD: 10, PriceUSD: 1.0,
			TradeTime: ts.Add(-10 * time.Second), ReceivedAt: ts,
		}
		insert(t, s, ev)
		entry := 1.0
		if err := s.CreateMarkouts(ev, storage.MarkoutFollower, &entry, []time.Duration{30 * time.Second}, now); err != nil {
			t.Fatal(err)
		}
		if err := s.FillMarkout(ev.ID, storage.MarkoutFollower, 30*time.Second, 1.0+ret/100, 0); err != nil {
			t.Fatal(err)
		}
	}
	mk("buy1", domain.Buy, 10)
	mk("sell1", domain.Sell, -20)

	var buf sink
	if err := LatencyEV(&buf, s, 30*time.Second, "buy", false, 100, 0); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	t.Log(out)

	// only the buy row may enter the 0-30s bucket: due=1 filled=1 cover=100%
	if !strings.Contains(out, "0-30s") || !strings.Contains(out, "100.0%") {
		t.Errorf("buy-only bucket wrong (sell row must be excluded): %s", out)
	}
	if strings.Contains(out, "+10.00%") && strings.Contains(out, "-20.00%") {
		t.Errorf("sell return leaked into buy-only EV: %s", out)
	}
}

// TestLatencyEVMeasurementFailureExcluded pins the measurement vs market
// split: an api_error row is coverage loss, NOT a -100% trade — it must not
// drag the conservative EV down.
func TestLatencyEVMeasurementFailureExcluded(t *testing.T) {
	s, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	mk := func(id string, status string, ret *float64) {
		ts := now.Add(-20 * time.Minute)
		ev := domain.TradeEvent{
			ID:     domain.EventID("sol", id, "W", "T_"+id, "buy"),
			Source: "gmgn_smartmoney", Chain: "sol", TxHash: id,
			Wallet: "W", WalletType: domain.WalletSmartMoney,
			TokenAddress: "T_" + id, Side: domain.Buy, AmountUSD: 10, PriceUSD: 1.0,
			TradeTime: ts.Add(-10 * time.Second), ReceivedAt: ts,
		}
		insert(t, s, ev)
		entry := 1.0
		if err := s.CreateMarkouts(ev, storage.MarkoutFollower, &entry, []time.Duration{30 * time.Second}, now); err != nil {
			t.Fatal(err)
		}
		if ret != nil {
			if err := s.FillMarkout(ev.ID, storage.MarkoutFollower, 30*time.Second, 1.0+*ret/100, 0); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := s.SetMarkoutStatus(ev.ID, storage.MarkoutFollower, 30*time.Second, status); err != nil {
				t.Fatal(err)
			}
		}
	}
	p10 := 10.0
	mk("e1", storage.MarkoutStatusFilled, &p10)
	mk("e2", storage.MarkoutStatusAPIError, nil) // measurement failure

	var buf sink
	if err := LatencyEV(&buf, s, 30*time.Second, "", false, 100, 0); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	t.Log(out)

	// due=2 filled=1 cover=50%, but cons EV = obs EV = +10 — the api_error
	// row is excluded from the denominator, not counted as -100%.
	if !strings.Contains(out, "50.0%") {
		t.Errorf("coverage must count the api_error row as due: %s", out)
	}
	if !strings.Contains(out, "+10.00%") {
		t.Errorf("cons EV wrong (want +10.00 — measurement failure excluded): %s", out)
	}
	if strings.Contains(out, "-20.00%") || strings.Contains(out, "-100.00%") {
		t.Errorf("api_error must NOT count as -100%% trade: %s", out)
	}
	// meas column reports the coverage loss separately
	if !strings.Contains(out, "meas") || !strings.Contains(out, "1") {
		t.Errorf("meas column missing: %s", out)
	}
}

// TestLatencyEVByChaseUnobserved pins that follower rows whose entry is
// unknown stay visible in the by-chase matrix (n/a column) instead of
// silently vanishing from the row totals.
func TestLatencyEVByChaseUnobserved(t *testing.T) {
	s, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	mk := func(id string, basePrice *float64, ret *float64) {
		ts := now.Add(-20 * time.Minute)
		ev := domain.TradeEvent{
			ID:     domain.EventID("sol", id, "W", "T_"+id, "buy"),
			Source: "gmgn_smartmoney", Chain: "sol", TxHash: id,
			Wallet: "W", WalletType: domain.WalletSmartMoney,
			TokenAddress: "T_" + id, Side: domain.Buy, AmountUSD: 10, PriceUSD: 1.0,
			TradeTime: ts.Add(-10 * time.Second), ReceivedAt: ts,
		}
		insert(t, s, ev)
		if err := s.CreateMarkouts(ev, storage.MarkoutFollower, basePrice, []time.Duration{30 * time.Second}, now); err != nil {
			t.Fatal(err)
		}
		if ret != nil {
			if err := s.FillMarkout(ev.ID, storage.MarkoutFollower, 30*time.Second, 1.0+*ret/100, 0); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := s.SetMarkoutStatus(ev.ID, storage.MarkoutFollower, 30*time.Second, storage.MarkoutStatusLookbackMiss); err != nil {
				t.Fatal(err)
			}
		}
	}
	entry := 1.0
	p10 := 10.0
	mk("e1", &entry, &p10) // chase computable (entry == leader price → 0-5%)
	mk("e2", nil, nil)     // entry unknown → n/a, must not vanish

	var buf sink
	if err := LatencyEV(&buf, s, 30*time.Second, "", true, 100, 0); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	t.Log(out)

	// totalN (due) for 0-30s must include BOTH rows: coverage = 1/2 = 50%.
	// If the unobserved row vanished, totalN would be 1 and coverage 100%.
	if !strings.Contains(out, "n/a") {
		t.Errorf("by-chase matrix missing n/a column: %s", out)
	}
	if !strings.Contains(out, "50.0%") {
		t.Errorf("unobserved row vanished from due total (want coverage 50.0%%): %s", out)
	}
}

// TestLatencyEVPendingDueUnresolved pins the P0.5 fix: a row that is DUE but
// still 'pending' (the worker has not classified it — tick lag, backlog) is
// sampling throughput, not a market outcome. It must not enter the cons-EV
// denominator as a -100% trade; it shows up in the unresolved column.
func TestLatencyEVPendingDueUnresolved(t *testing.T) {
	s, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	mk := func(id string, ret *float64) {
		ts := now.Add(-20 * time.Minute)
		ev := domain.TradeEvent{
			ID:     domain.EventID("sol", id, "W", "T_"+id, "buy"),
			Source: "gmgn_smartmoney", Chain: "sol", TxHash: id,
			Wallet: "W", WalletType: domain.WalletSmartMoney,
			TokenAddress: "T_" + id, Side: domain.Buy, AmountUSD: 10, PriceUSD: 1.0,
			TradeTime: ts.Add(-10 * time.Second), ReceivedAt: ts,
		}
		insert(t, s, ev)
		entry := 1.0
		if err := s.CreateMarkouts(ev, storage.MarkoutFollower, &entry, []time.Duration{30 * time.Second}, now); err != nil {
			t.Fatal(err)
		}
		if ret != nil {
			if err := s.FillMarkout(ev.ID, storage.MarkoutFollower, 30*time.Second, 1.0+*ret/100, 0); err != nil {
				t.Fatal(err)
			}
		}
		// no status set for e2 → stays 'pending' although already due
	}
	p10 := 10.0
	mk("e1", &p10)
	mk("e2", nil) // due but pending (worker lag)

	var buf sink
	if err := LatencyEV(&buf, s, 30*time.Second, "", false, 100, 0); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	t.Log(out)

	// due=2 filled=1 cover=50%, but cons EV = obs EV = +10: the pending row
	// is unresolved, not a -100% market loss. If it wrongly entered the
	// denominator, cons would be (10-100)/2 = -45.00.
	if !strings.Contains(out, "50.0%") {
		t.Errorf("coverage must count the pending row as due: %s", out)
	}
	if !strings.Contains(out, "+10.00%") {
		t.Errorf("cons EV wrong (want +10.00 — pending row excluded): %s", out)
	}
	if strings.Contains(out, "-45.00%") {
		t.Errorf("pending-due row must NOT count as -100%% trade: %s", out)
	}
	if !strings.Contains(out, "unresolved") {
		t.Errorf("unresolved column missing: %s", out)
	}
}

// TestCoverageSplitsPending pins the P1 fix: `analyze coverage` must show
// due-but-unclassified rows as unresolved_due (worker lag) separately from
// not-yet-due rows — previously both were merged under one 'pending' line.
func TestCoverageSplitsPending(t *testing.T) {
	s, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	// E1: due + filled (20m ago)
	// E2: due + pending (20m ago, worker never classified)
	// E3: NOT due yet (created just now) → pending but not_due
	mk := func(id string, ago time.Duration, fill bool) {
		ts := now.Add(-ago)
		ev := domain.TradeEvent{
			ID:     domain.EventID("sol", id, "W", "T_"+id, "buy"),
			Source: "gmgn_smartmoney", Chain: "sol", TxHash: id,
			Wallet: "W", WalletType: domain.WalletSmartMoney,
			TokenAddress: "T_" + id, Side: domain.Buy, AmountUSD: 10, PriceUSD: 1.0,
			TradeTime: ts.Add(-10 * time.Second), ReceivedAt: ts,
		}
		insert(t, s, ev)
		entry := 1.0
		if err := s.CreateMarkouts(ev, storage.MarkoutFollower, &entry, []time.Duration{30 * time.Second}, now); err != nil {
			t.Fatal(err)
		}
		if fill {
			if err := s.FillMarkout(ev.ID, storage.MarkoutFollower, 30*time.Second, 1.1, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("e1", 20*time.Minute, true)
	mk("e2", 20*time.Minute, false) // due, status stays pending
	mk("e3", 0, false)              // just created → horizon not reached

	var buf sink
	if err := Coverage(&buf, s, storage.MarkoutFollower, []time.Duration{30 * time.Second}, 0); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	t.Log(out)

	if !strings.Contains(out, "unresolved_due") || !strings.Contains(out, "not_due") {
		t.Fatalf("coverage must split unresolved_due / not_due: %s", out)
	}
	// 2 due rows (e1 filled + e2 unresolved), 1 not-due (e3)
	if !regexp.MustCompile(`unresolved_due\s+1\s`).MatchString(out) {
		t.Errorf("unresolved_due must be 1 (the due-pending row): %s", out)
	}
	if !regexp.MustCompile(`not_due\s+1\s`).MatchString(out) {
		t.Errorf("not_due must be 1 (the just-created row): %s", out)
	}
}
