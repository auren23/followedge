package markout

import (
	"context"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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
	if err := s.CreateMarkouts(ev, storage.MarkoutLeader, &ev.PriceUSD, []time.Duration{30 * time.Second}, now); err != nil {
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
	cs := []sampledCandle{
		{open: 100, close: 1.0, valid: true},
		{open: 130, close: 1.1, valid: true},
		{open: 160, close: 1.2, valid: true},
		{open: 190, close: 1.3, valid: true},
	}
	const res = int64(30)

	cases := []struct {
		ts   int64
		want float64
		ok   bool
	}{
		{100, 1.0, true}, // close of open=100 candle (100+30) is exactly ts
		{115, 1.0, true}, // open=100 (close 130) is already >= 115
		{131, 1.1, true}, // first close >= 131 is open=130 (close 160)
		{191, 1.3, true}, // last candle (open=190, close=220)
		{221, 0, false},  // past the feed (last close = 220)
		{99, 1.0, true},  // before first candle
		{159, 1.1, true}, // open=130 close=160 is the first close >= 159
	}
	for _, c := range cases {
		got, ok := firstCandleClosingAtOrAfter(cs, c.ts, res)
		if got.close != c.want || ok != c.ok {
			t.Errorf("firstCandleClosingAtOrAfter(%d) = (%v, %v), want (%v, %v)", c.ts, got, ok, c.want, c.ok)
		}
	}

	// invalid candle is found but flagged: parse failure must NOT look like
	// a dead token (found=true) nor like a valid price (valid=false).
	bad := []sampledCandle{{open: 100, close: 0, valid: false}}
	got, found := firstCandleClosingAtOrAfter(bad, 100, res)
	if !found || got.valid {
		t.Errorf("unparseable candle: found=%v valid=%v, want found=true valid=false", found, got.valid)
	}
	if _, found := lastCloseAtOrBefore(bad, 100); !found {
		t.Errorf("lastCloseAtOrBefore must find the unparseable candle (entry parse error path)")
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

// recordingClient wraps fakeClient and records the kline window it was asked
// for, so tests can assert the lookback starts exactly 2 resolutions before
// the earliest due time.
type recordingClient struct {
	fake fakeClient
	from time.Time
}

func (r *recordingClient) Kline(ctx context.Context, chain, address, resolution string, from, to time.Time) ([]gmgn.Candle, error) {
	r.from = from
	return r.fake.Kline(ctx, chain, address, resolution, from, to)
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
	price := ev.PriceUSD
	if err := s.CreateMarkouts(ev, storage.MarkoutLeader, &price, []time.Duration{30 * time.Second, 1 * time.Minute}, now); err != nil {
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

	rows, err := s.MarkoutsAt(storage.MarkoutLeader, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].ReturnPct.Valid {
		t.Fatalf("expected 1 filled 30s leader markout, got %+v", rows)
	}
	// due = trade_time+30s; observed = close of the first candle whose CLOSE
	// time (open+30s) is >= due — i.e. the first candle opening at/after
	// due-30s. Recompute the expectation from the same arrays instead of
	// hardcoding (candle grid depends on truncation of `now`).
	wantIdx := sort.Search(len(fc.candles), func(i int) bool {
		return fc.candles[i].Time/1000 >= ev.TradeTime.Add(30*time.Second).Add(-30*time.Second).Unix()
	})
	want, _ := strconv.ParseFloat(fc.candles[wantIdx].Close, 64)
	wantRet := (want/ev.PriceUSD - 1) * 100
	if got := rows[0].ReturnPct.Float64; got != wantRet {
		t.Errorf("30s markout return = %.2f%%, want %.2f%%", got, wantRet)
	}
}

// TestSampleDueLookback pins the P1 fix: the kline window must start 2
// resolutions before the earliest due time (min horizon 1 res + 1 closed
// candle for the follower entry), not the hardcoded 1 resolution.
func TestSampleDueLookback(t *testing.T) {
	s, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	received := now.Add(-5 * time.Minute)
	ev := domain.TradeEvent{
		ID:     domain.EventID("sol", "txlb", "W1", "TOKEN_LB", "buy"),
		Source: "gmgn_smartmoney", Chain: "sol", TxHash: "txlb",
		Wallet: "W1", WalletType: domain.WalletSmartMoney,
		TokenAddress: "TOKEN_LB", Side: domain.Buy, AmountUSD: 100, PriceUSD: 1.00,
		TradeTime: received, ReceivedAt: received,
	}
	created, err := s.InsertEvent(ev)
	if err != nil || !created {
		t.Fatalf("insert: %v %v", created, err)
	}
	if err := s.CreateMarkouts(ev, storage.MarkoutFollower, nil, []time.Duration{30 * time.Second}, now); err != nil {
		t.Fatal(err)
	}

	// entry candle at ReceivedAt-30s (closes at ReceivedAt), exit candle at
	// ReceivedAt+30s (due).
	b := received.Truncate(30 * time.Second)
	rc := &recordingClient{fake: fakeClient{candles: []gmgn.Candle{
		{Time: b.Add(-30 * time.Second).UnixMilli(), Close: "1.00"},
		{Time: b.UnixMilli(), Close: "1.00"},
		{Time: b.Add(30 * time.Second).UnixMilli(), Close: "1.10"},
	}}}
	eng := NewEngine(s, nil, noopLimiter{}, "sol", "30s", 0, []time.Duration{30 * time.Second})
	eng.client = rc
	if err := eng.SampleDue(context.Background()); err != nil {
		t.Fatal(err)
	}

	// earliest due = ReceivedAt(sec-truncated) + 30s → window starts at
	// ReceivedAt(sec-truncated) − 30s (2 resolutions before earliest due)
	wantFrom := time.Unix(received.Unix()-30, 0)
	if !rc.from.Equal(wantFrom) {
		t.Errorf("kline window starts %v, want %v (2 resolutions before earliest due)", rc.from, wantFrom)
	}
}

// TestSampleDueStatuses pins every failure branch classifies its rows:
// empty candles → token_inactive, candle stream ending before the horizon →
// no_candle, follower entry out of window → lookback_miss.
func TestSampleDueStatuses(t *testing.T) {
	s, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	mkLeader := func(id, token string) {
		base := now.Add(-5 * time.Minute).Truncate(30 * time.Second).Add(15 * time.Second)
		ev := domain.TradeEvent{
			ID:     domain.EventID("sol", id, "W1", token, "buy"),
			Source: "gmgn_smartmoney", Chain: "sol", TxHash: id,
			Wallet: "W1", WalletType: domain.WalletSmartMoney,
			TokenAddress: token, Side: domain.Buy, AmountUSD: 100, PriceUSD: 1.00,
			TradeTime: base, ReceivedAt: base,
		}
		created, err := s.InsertEvent(ev)
		if err != nil || !created {
			t.Fatalf("insert %s: %v %v", id, created, err)
		}
		price := ev.PriceUSD
		if err := s.CreateMarkouts(ev, storage.MarkoutLeader, &price, []time.Duration{30 * time.Second}, now); err != nil {
			t.Fatal(err)
		}
	}
	mkFollower := func(id, token string) {
		base := now.Add(-5 * time.Minute).Truncate(30 * time.Second).Add(15 * time.Second)
		ev := domain.TradeEvent{
			ID:     domain.EventID("sol", id, "W1", token, "buy"),
			Source: "gmgn_smartmoney", Chain: "sol", TxHash: id,
			Wallet: "W1", WalletType: domain.WalletSmartMoney,
			TokenAddress: token, Side: domain.Buy, AmountUSD: 100, PriceUSD: 1.00,
			TradeTime: base, ReceivedAt: base,
		}
		created, err := s.InsertEvent(ev)
		if err != nil || !created {
			t.Fatalf("insert %s: %v %v", id, created, err)
		}
		if err := s.CreateMarkouts(ev, storage.MarkoutFollower, nil, []time.Duration{30 * time.Second}, now); err != nil {
			t.Fatal(err)
		}
	}

	// TOKEN_A: no kline at all → token_inactive
	mkLeader("a1", "TOKEN_A")
	// TOKEN_B: candle stream ends before the horizon → no_candle
	mkLeader("b1", "TOKEN_B")
	b := now.Add(-5 * time.Minute).Truncate(30 * time.Second)
	// TOKEN_C: follower entry candle out of window → lookback_miss
	mkFollower("c1", "TOKEN_C")
	// TOKEN_D: healthy → filled (control)
	mkLeader("d1", "TOKEN_D")

	eng := NewEngine(s, nil, noopLimiter{}, "sol", "30s", 0, []time.Duration{30 * time.Second})
	eng.client = &tokenCannedClient{byToken: map[string][]gmgn.Candle{
		// B: only one candle (closes b+30s < due b+45s) → no close at/after due
		"TOKEN_B": {
			{Time: b.UnixMilli(), Close: "1.00"},
		},
		// C: candles only start at base+60s → entry (needs open <= base−30s) missing
		"TOKEN_C": {
			{Time: b.Add(60 * time.Second).UnixMilli(), Close: "1.20"},
		},
		// D: full stream, fills normally
		"TOKEN_D": {
			{Time: b.UnixMilli(), Close: "1.00"},
			{Time: b.Add(30 * time.Second).UnixMilli(), Close: "1.10"},
			{Time: b.Add(60 * time.Second).UnixMilli(), Close: "1.20"},
		},
	}}
	if err := eng.SampleDue(context.Background()); err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"TOKEN_A": storage.MarkoutStatusNoKlineData,
		"TOKEN_B": storage.MarkoutStatusNoCandle,
		"TOKEN_C": storage.MarkoutStatusLookbackMiss,
	}
	for token, st := range want {
		var got string
		err := s.DB().QueryRow(`
			SELECT m.status FROM markouts m
			JOIN trade_events e ON e.event_id = m.event_id
			WHERE e.token_address = ? AND m.horizon_ms = 30000`, token).Scan(&got)
		if err != nil {
			t.Fatalf("%s: %v", token, err)
		}
		if got != st {
			t.Errorf("%s status = %s, want %s", token, got, st)
		}
	}
	// backoff: a second pass must NOT re-request the empty-kline token
	// (no_kline_data is retryable in the queue but skipUntil gates it);
	// TOKEN_C (lookback_miss) IS retried; TOKEN_D is already filled and
	// correctly absent from the queue.
	counted := &countingClient{c: eng.client}
	eng.client = counted
	if err := eng.SampleDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if counted.requests["TOKEN_A"] != 0 {
		t.Errorf("empty-kline token re-requested within backoff: %d calls", counted.requests["TOKEN_A"])
	}
	if counted.requests["TOKEN_C"] != 1 {
		t.Errorf("retryable TOKEN_C should be sampled again: %d calls", counted.requests["TOKEN_C"])
	}
	if counted.requests["TOKEN_D"] != 0 {
		t.Errorf("filled TOKEN_D must not be resampled: %d calls", counted.requests["TOKEN_D"])
	}
	// control: TOKEN_D filled
	rows, err := s.MarkoutsAt(storage.MarkoutLeader, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].ReturnPct.Valid {
		t.Errorf("control TOKEN_D not filled: %+v", rows)
	}
}

// tokenCannedClient serves per-token candle sets (empty map entry = empty stream).
type tokenCannedClient struct {
	byToken map[string][]gmgn.Candle
}

func (t *tokenCannedClient) Kline(ctx context.Context, chain, address, resolution string, from, to time.Time) ([]gmgn.Candle, error) {
	return t.byToken[address], nil
}

// countingClient wraps a klineFetcher and records per-token request counts.
type countingClient struct {
	c        klineFetcher
	requests map[string]int
}

func (c *countingClient) Kline(ctx context.Context, chain, address, resolution string, from, to time.Time) ([]gmgn.Candle, error) {
	if c.requests == nil {
		c.requests = map[string]int{}
	}
	c.requests[address]++
	return c.c.Kline(ctx, chain, address, resolution, from, to)
}

// TestSampleDueFollower verifies the measurement split AND the P0.5 fix:
// the entry must be the last candle ALREADY CLOSED at ReceivedAt. The candle
// in progress at reception (close = 1.10) contains up to 30s of future info
// and must NOT be used — a look-ahead would make chase +10% and fail this
// test.
func TestSampleDueFollower(t *testing.T) {
	s, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	tradeTime := now.Add(-7 * time.Minute)
	// fixed 15s offset from the 30s boundary: keeps the second-truncated
	// base_ms (ReceivedAt.Unix()) unambiguous for entry/exit candle search.
	received := now.Add(-5 * time.Minute).Truncate(30 * time.Second).Add(15 * time.Second) // 2-minute GMGN delay
	ev := domain.TradeEvent{
		ID:     domain.EventID("sol", "txf", "W1", "TOKEN_F", "buy"),
		Source: "gmgn_smartmoney", Chain: "sol", TxHash: "txf",
		Wallet: "W1", WalletType: domain.WalletSmartMoney,
		TokenAddress: "TOKEN_F", Side: domain.Buy, AmountUSD: 100, PriceUSD: 1.00,
		TradeTime: tradeTime, ReceivedAt: received,
	}
	created, err := s.InsertEvent(ev)
	if err != nil || !created {
		t.Fatalf("insert: %v %v", created, err)
	}
	if err := s.CreateMarkouts(ev, storage.MarkoutFollower, nil, []time.Duration{30 * time.Second}, now); err != nil {
		t.Fatal(err)
	}

	// 30s candles around reception (received = 16:50:07 in the comment grid):
	//   c0 open=16:49:30 close=1.00  → closed at 16:50:00 <= received ✓ (entry)
	//   c1 open=16:50:00 close=1.10  → closes at 16:50:30 > received ✗ in progress
	//   c2 open=16:50:30 close=1.30  → closes at 16:51:00 >= due(16:50:37) ✓ (exit,
	//      first candle whose CLOSE time reaches the horizon; lag 23s)
	boundary := received.Truncate(30 * time.Second)
	c0 := boundary.Add(-30 * time.Second)
	fc := &fakeClient{candles: []gmgn.Candle{
		{Time: c0.UnixMilli(), Close: "1.00"},
		{Time: boundary.UnixMilli(), Close: "1.10"}, // trap: must NOT be used
		{Time: boundary.Add(30 * time.Second).UnixMilli(), Close: "1.30"},
	}}
	eng := NewEngine(s, nil, noopLimiter{}, "sol", "30s", 0, []time.Duration{30 * time.Second})
	eng.client = fc
	if err := eng.SampleDue(context.Background()); err != nil {
		t.Fatal(err)
	}

	rows, err := s.MarkoutsAt(storage.MarkoutFollower, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].ReturnPct.Valid {
		t.Fatalf("expected 1 filled follower markout, got %+v", rows)
	}
	m := rows[0]
	if m.BasePrice != 1.00 {
		t.Errorf("follower entry price = %v, want 1.00 (last closed candle at reception, NOT the in-progress 1.10)", m.BasePrice)
	}
	if math.Abs(m.ChasePct) > 0.001 {
		t.Errorf("chase = %.4f%%, want ~0%% (entry 1.00 vs leader 1.00)", m.ChasePct)
	}
	wantRet := (1.30/1.00 - 1) * 100
	if math.Abs(m.ReturnPct.Float64-wantRet) > 0.001 {
		t.Errorf("follower return = %.4f%%, want ~%.4f%% (from entry, not leader price)", m.ReturnPct.Float64, wantRet)
	}
	if m.EntryObservedAt != boundary.Unix() {
		t.Errorf("entry observed at = %d, want %d (candle close instant, no future info)", m.EntryObservedAt, boundary.Unix())
	}
	if m.LeaderPrice != 1.00 {
		t.Errorf("leader price = %v, want 1.00", m.LeaderPrice)
	}
}

// TestSampleDueParseError pins the price_parse_error path: a candle whose
// close cannot be parsed is a MEASUREMENT failure — it must not fall into
// lookback_miss (entry) or no_candle (horizon), i.e. must not look like a
// dead token.
func TestSampleDueParseError(t *testing.T) {
	s, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	mkLeader := func(id, token string) {
		base := now.Add(-5 * time.Minute).Truncate(30 * time.Second).Add(15 * time.Second)
		ev := domain.TradeEvent{
			ID:     domain.EventID("sol", id, "W1", token, "buy"),
			Source: "gmgn_smartmoney", Chain: "sol", TxHash: id,
			Wallet: "W1", WalletType: domain.WalletSmartMoney,
			TokenAddress: token, Side: domain.Buy, AmountUSD: 100, PriceUSD: 1.00,
			TradeTime: base, ReceivedAt: base,
		}
		if _, err := s.InsertEvent(ev); err != nil {
			t.Fatal(err)
		}
		price := ev.PriceUSD
		if err := s.CreateMarkouts(ev, storage.MarkoutLeader, &price, []time.Duration{30 * time.Second}, now); err != nil {
			t.Fatal(err)
		}
	}
	mkFollower := func(id, token string) {
		base := now.Add(-5 * time.Minute).Truncate(30 * time.Second).Add(15 * time.Second)
		ev := domain.TradeEvent{
			ID:     domain.EventID("sol", id, "W1", token, "buy"),
			Source: "gmgn_smartmoney", Chain: "sol", TxHash: id,
			Wallet: "W1", WalletType: domain.WalletSmartMoney,
			TokenAddress: token, Side: domain.Buy, AmountUSD: 100, PriceUSD: 1.00,
			TradeTime: base, ReceivedAt: base,
		}
		if _, err := s.InsertEvent(ev); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateMarkouts(ev, storage.MarkoutFollower, nil, []time.Duration{30 * time.Second}, now); err != nil {
			t.Fatal(err)
		}
	}

	// P1: horizon candle exists but close is garbage (open b+60 >= due b+45)
	mkLeader("p1", "TOKEN_P1")
	// P2: entry candle exists but close is garbage (open b-30 <= base-30)
	mkFollower("p2", "TOKEN_P2")
	b := now.Add(-5 * time.Minute).Truncate(30 * time.Second)

	eng := NewEngine(s, nil, noopLimiter{}, "sol", "30s", 0, []time.Duration{30 * time.Second})
	eng.client = &tokenCannedClient{byToken: map[string][]gmgn.Candle{
		"TOKEN_P1": {{Time: b.UnixMilli(), Close: "1.00"}, {Time: b.Add(60 * time.Second).UnixMilli(), Close: "garbage"}},
		"TOKEN_P2": {{Time: b.Add(-30 * time.Second).UnixMilli(), Close: "garbage"}, {Time: b.Add(60 * time.Second).UnixMilli(), Close: "1.10"}},
	}}
	if err := eng.SampleDue(context.Background()); err != nil {
		t.Fatal(err)
	}

	for _, token := range []string{"TOKEN_P1", "TOKEN_P2"} {
		var st string
		if err := s.DB().QueryRow(`SELECT m.status FROM markouts m JOIN trade_events e ON e.event_id = m.event_id WHERE e.token_address = ?`, token).Scan(&st); err != nil {
			t.Fatal(err)
		}
		if st != storage.MarkoutStatusParseError {
			t.Errorf("%s status = %s, want %s (malformed close is measurement failure, not dead token)", token, st, storage.MarkoutStatusParseError)
		}
	}
}

// TestSampleDueStaleOutcome pins the fixed-horizon rule: the first candle at
// or after due must open within [due, due+res] to count as a strict fill;
// otherwise the row is stale_outcome. Valid fills record outcome_observed_at.
func TestSampleDueStaleOutcome(t *testing.T) {
	s, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	mkLeader := func(id, token string) {
		base := now.Add(-5 * time.Minute).Truncate(30 * time.Second).Add(15 * time.Second)
		ev := domain.TradeEvent{
			ID:     domain.EventID("sol", id, "W1", token, "buy"),
			Source: "gmgn_smartmoney", Chain: "sol", TxHash: id,
			Wallet: "W1", WalletType: domain.WalletSmartMoney,
			TokenAddress: token, Side: domain.Buy, AmountUSD: 100, PriceUSD: 1.00,
			TradeTime: base, ReceivedAt: base,
		}
		if _, err := s.InsertEvent(ev); err != nil {
			t.Fatal(err)
		}
		price := ev.PriceUSD
		if err := s.CreateMarkouts(ev, storage.MarkoutLeader, &price, []time.Duration{30 * time.Second}, now); err != nil {
			t.Fatal(err)
		}
	}

	// S1: due = b+45s, next candle close = b+120s (> due+30s) → stale_outcome
	mkLeader("s1", "TOKEN_S1")
	// S2: due = b+45s, next candle close = b+60s (<= due+30s) → strict fill
	mkLeader("s2", "TOKEN_S2")
	b := now.Add(-5 * time.Minute).Truncate(30 * time.Second)

	eng := NewEngine(s, nil, noopLimiter{}, "sol", "30s", 0, []time.Duration{30 * time.Second})
	eng.client = &tokenCannedClient{byToken: map[string][]gmgn.Candle{
		"TOKEN_S1": {{Time: b.UnixMilli(), Close: "1.00"}, {Time: b.Add(90 * time.Second).UnixMilli(), Close: "1.10"}},
		"TOKEN_S2": {{Time: b.UnixMilli(), Close: "1.00"}, {Time: b.Add(30 * time.Second).UnixMilli(), Close: "1.10"}},
	}}
	if err := eng.SampleDue(context.Background()); err != nil {
		t.Fatal(err)
	}

	var st string
	if err := s.DB().QueryRow(`SELECT m.status FROM markouts m JOIN trade_events e ON e.event_id = m.event_id WHERE e.token_address = 'TOKEN_S1'`).Scan(&st); err != nil {
		t.Fatal(err)
	}
	if st != storage.MarkoutStatusStaleOutcome {
		t.Errorf("TOKEN_S1 status = %s, want %s (first close landed past due+res)", st, storage.MarkoutStatusStaleOutcome)
	}

	rows, err := s.MarkoutsAt(storage.MarkoutLeader, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].ReturnPct.Valid {
		t.Fatalf("expected exactly the strict fill TOKEN_S2, got %+v", rows)
	}
	// observed at = candle close instant = open + res
	wantObs := b.Add(30*time.Second).Unix() + 30
	if rows[0].OutcomeObservedAt != wantObs {
		t.Errorf("outcome_observed_at = %d, want %d (candle close instant)", rows[0].OutcomeObservedAt, wantObs)
	}
}

// TestDueMarkoutsExcludesTerminal pins the P0.5 fix: rows whose market
// outcome is final (no_candle / token_inactive / stale_outcome) never fill
// again — they must not be re-listed every tick, or kline quota burns on
// rows that can never succeed. Only pending/transient rows stay eligible.
func TestDueMarkoutsExcludesTerminal(t *testing.T) {
	s, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	mk := func(id string, status string) {
		base := now.Add(-10 * time.Minute).Truncate(30 * time.Second)
		ev := domain.TradeEvent{
			ID:     domain.EventID("sol", id, "W1", "T_"+id, "buy"),
			Source: "gmgn_smartmoney", Chain: "sol", TxHash: id,
			Wallet: "W1", WalletType: domain.WalletSmartMoney,
			TokenAddress: "T_" + id, Side: domain.Buy, AmountUSD: 100, PriceUSD: 1.00,
			TradeTime: base, ReceivedAt: base,
		}
		if _, err := s.InsertEvent(ev); err != nil {
			t.Fatal(err)
		}
		price := ev.PriceUSD
		if err := s.CreateMarkouts(ev, storage.MarkoutLeader, &price, []time.Duration{30 * time.Second}, now); err != nil {
			t.Fatal(err)
		}
		if status != "" {
			if err := s.SetMarkoutStatus(ev.ID, storage.MarkoutLeader, 30*time.Second, status); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("a", "")                                 // pending (eligible)
	mk("b", storage.MarkoutStatusNoCandle)      // terminal
	mk("c", storage.MarkoutStatusTokenInactive) // terminal
	mk("d", storage.MarkoutStatusStaleOutcome)  // terminal
	mk("e", storage.MarkoutStatusLookbackMiss)  // retryable (eligible)

	due, err := s.DueMarkouts(0, now, 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range due {
		got = append(got, d.Token)
	}
	sort.Strings(got)
	want := []string{"T_a", "T_e"} // only pending + retryable
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("DueMarkouts returned %v, want %v (terminal statuses must be excluded)", got, want)
	}
}

// TestSampleDueCloseTimeHorizon pins the close-time horizon rule: the exit
// price is the close of the FIRST candle whose CLOSE time reaches the due
// time, not the first candle whose OPEN is >= due. With due = b+45s and
// candles opening at b, b+30, b+60 the open-based search would take the
// b+60 candle (price at b+90, 45s late); the close-time search takes b+30
// (price at b+60, 15s late — lag ∈ [0, res]).
func TestSampleDueCloseTimeHorizon(t *testing.T) {
	s, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	base := now.Add(-5 * time.Minute).Truncate(30 * time.Second).Add(15 * time.Second)
	ev := domain.TradeEvent{
		ID:     domain.EventID("sol", "cth", "W1", "TOKEN_CTH", "buy"),
		Source: "gmgn_smartmoney", Chain: "sol", TxHash: "cth",
		Wallet: "W1", WalletType: domain.WalletSmartMoney,
		TokenAddress: "TOKEN_CTH", Side: domain.Buy, AmountUSD: 100, PriceUSD: 1.00,
		TradeTime: base, ReceivedAt: base,
	}
	if _, err := s.InsertEvent(ev); err != nil {
		t.Fatal(err)
	}
	price := ev.PriceUSD
	if err := s.CreateMarkouts(ev, storage.MarkoutLeader, &price, []time.Duration{30 * time.Second}, now); err != nil {
		t.Fatal(err)
	}

	b := base.Truncate(30 * time.Second) // due = base+30s = b+45s
	eng := NewEngine(s, nil, noopLimiter{}, "sol", "30s", 0, []time.Duration{30 * time.Second})
	eng.client = &tokenCannedClient{byToken: map[string][]gmgn.Candle{
		"TOKEN_CTH": {
			{Time: b.UnixMilli(), Close: "1.00"},
			{Time: b.Add(30 * time.Second).UnixMilli(), Close: "1.10"}, // closes b+60 >= due b+45 → exit
			{Time: b.Add(60 * time.Second).UnixMilli(), Close: "1.20"}, // trap: open >= due, but 45s late
		},
	}}
	if err := eng.SampleDue(context.Background()); err != nil {
		t.Fatal(err)
	}

	rows, err := s.MarkoutsAt(storage.MarkoutLeader, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].ReturnPct.Valid {
		t.Fatalf("expected 1 filled markout, got %+v", rows)
	}
	// exit price 1.10 (b+30 candle), NOT the open-based 1.20
	if math.Abs(rows[0].ReturnPct.Float64-(1.10/1.00-1)*100) > 0.001 {
		t.Errorf("return = %.4f%%, want 10%% (close-time horizon must pick the b+30 candle, not b+60)", rows[0].ReturnPct.Float64)
	}
	// observation instant = candle close = b+60; lag vs due (b+45) = 15s <= res
	wantObs := b.Add(30*time.Second).Unix() + 30
	if rows[0].OutcomeObservedAt != wantObs {
		t.Errorf("outcome_observed_at = %d, want %d (close time, lag must be in [0, res])", rows[0].OutcomeObservedAt, wantObs)
	}
}
