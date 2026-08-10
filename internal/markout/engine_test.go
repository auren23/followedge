package markout

import (
	"context"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/auren23/followedge/internal/domain"
	"github.com/auren23/followedge/internal/source/gmgn"
	"github.com/auren23/followedge/internal/storage"
)

// noopLimiter lets tests drive the sampler without throttling.
type noopLimiter struct{}

func (noopLimiter) Take(ctx context.Context, weight float64) error { return nil }
func (noopLimiter) MarkCooldown(resetAt time.Time)                 {}

// spyLimiter records cooldown signals so the test can assert a 429 froze
// the pipeline instead of being retried.
type spyLimiter struct {
	cooldowns int
}

func (s *spyLimiter) Take(ctx context.Context, weight float64) error { return nil }
func (s *spyLimiter) MarkCooldown(resetAt time.Time)                 { s.cooldowns++ }

func TestSampleDueRateLimitFreezesPipeline(t *testing.T) {
	s, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	ev := domain.TradeEvent{
		ID:     domain.EventID("sol", "tx9", "W9", "TOKEN_RL", "buy"),
		Source: "gmgn_smartmoney", Chain: "sol", TxHash: "tx9",
		Wallet: "W9", WalletType: domain.WalletSmartMoney,
		TokenAddress: "TOKEN_RL", Side: domain.Buy, AmountUSD: 100, PriceUSD: 1.0,
		TradeTime: now.Add(-5 * time.Minute), ReceivedAt: now.Add(-5 * time.Minute),
	}
	created, err := s.InsertEvent(ev)
	if err != nil || !created {
		t.Fatalf("insert: %v %v", created, err)
	}
	if err := s.CreateMarkouts(ev, []time.Duration{30 * time.Second}, now); err != nil {
		t.Fatal(err)
	}

	// the fake kline source answers with a rate-limit error
	rlClient := &rateLimitedClient{reset: now.Add(60 * time.Second)}
	spy := &spyLimiter{}
	eng := NewEngine(s, nil, spy, "sol", "30s", 0, []time.Duration{30 * time.Second})
	eng.client = rlClient

	if err := eng.SampleDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if spy.cooldowns != 1 {
		t.Fatalf("expected 1 MarkCooldown call, got %d — the 429 must freeze the pipeline", spy.cooldowns)
	}
}

type rateLimitedClient struct {
	reset time.Time
}

func (r *rateLimitedClient) Kline(ctx context.Context, chain, address, resolution string, from, to time.Time) ([]gmgn.Candle, error) {
	return nil, &gmgn.RateLimitError{ResetAt: r.reset}
}

func TestFirstCloseAtOrAfter(t *testing.T) {
	times := []int64{100, 130, 160, 190}
	closes := []float64{1.0, 1.1, 1.2, 1.3}

	cases := []struct {
		ts   int64
		want float64
		ok   bool
	}{
		{100, 1.0, true}, // exact open time
		{115, 1.1, true}, // between candles
		{190, 1.3, true}, // last candle
		{191, 0, false},  // past the feed
		{99, 1.0, true},  // before first candle
	}
	for _, c := range cases {
		got, ok := firstCloseAtOrAfter(times, closes, c.ts)
		if got != c.want || ok != c.ok {
			t.Errorf("firstCloseAtOrAfter(%d) = (%v, %v), want (%v, %v)", c.ts, got, ok, c.want, c.ok)
		}
	}
}

func TestParseHorizonsSorted(t *testing.T) {
	hs, err := ParseHorizons([]string{"1h", "30s", "5m"})
	if err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{30 * time.Second, 5 * time.Minute, time.Hour}
	for i := range want {
		if hs[i] != want[i] {
			t.Fatalf("horizons[%d] = %v, want %v", i, hs[i], want[i])
		}
	}
	if _, err := ParseHorizons([]string{"nonsense"}); err == nil {
		t.Fatal("expected parse error")
	}
}

// fakeClient serves canned kline candles.
type fakeClient struct {
	candles []gmgn.Candle
}

func (f *fakeClient) Kline(ctx context.Context, chain, address, resolution string, from, to time.Time) ([]gmgn.Candle, error) {
	return f.candles, nil
}

func TestSampleDue(t *testing.T) {
	s, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	ev := domain.TradeEvent{
		ID:     domain.EventID("sol", "tx1", "W1", "TOKEN_X", "buy"),
		Source: "gmgn_smartmoney", Chain: "sol", TxHash: "tx1",
		Wallet: "W1", WalletType: domain.WalletSmartMoney,
		TokenAddress: "TOKEN_X", Side: domain.Buy, AmountUSD: 100, PriceUSD: 1.00,
		TradeTime: now.Add(-5 * time.Minute), ReceivedAt: now.Add(-5 * time.Minute),
	}
	created, err := s.InsertEvent(ev)
	if err != nil || !created {
		t.Fatalf("insert: %v %v", created, err)
	}
	if err := s.CreateMarkouts(ev, []time.Duration{30 * time.Second, 1 * time.Minute}, now); err != nil {
		t.Fatal(err)
	}

	// event at T-5m; 30s markout due at T-4m30s, 1m markout due at T-4m.
	// candles: open 10:00:00 (price 1.00), 10:01:00 (1.10) ...
	base := ev.TradeTime.Add(-30 * time.Second).Truncate(30 * time.Second)
	fc := &fakeClient{candles: []gmgn.Candle{}}
	for i := 0; i < 12; i++ {
		fc.candles = append(fc.candles, gmgn.Candle{
			Time:  base.Add(time.Duration(i*30) * time.Second).UnixMilli(),
			Close: strconv.FormatFloat(1.00+0.1*float64(i), 'f', -1, 64),
		})
	}
	// engine over fake client
	eng := NewEngine(s, nil, noopLimiter{}, "sol", "30s", 0, []time.Duration{30 * time.Second, time.Minute})
	eng.client = fc

	if err := eng.SampleDue(context.Background()); err != nil {
		t.Fatal(err)
	}

	rows, err := s.MarkoutsAt(30 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].ReturnPct.Valid {
		t.Fatalf("expected 1 filled 30s markout, got %+v", rows)
	}
	// due = trade_time+30s; observed = close of first candle at/after due.
	// Recompute the expectation from the same arrays instead of hardcoding
	// (candle grid depends on truncation of `now`).
	wantIdx := sort.Search(len(fc.candles), func(i int) bool {
		return fc.candles[i].Time/1000 >= ev.TradeTime.Add(30*time.Second).Unix()
	})
	want, _ := strconv.ParseFloat(fc.candles[wantIdx].Close, 64)
	wantRet := (want/ev.PriceUSD - 1) * 100
	if got := rows[0].ReturnPct.Float64; got != wantRet {
		t.Errorf("30s markout return = %.2f%%, want %.2f%%", got, wantRet)
	}
}
