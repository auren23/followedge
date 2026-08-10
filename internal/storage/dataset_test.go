package storage

import (
	"database/sql"
	"math"
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
	if err := s.FillMarkout(ev.ID, MarkoutLeader, 30*time.Second, 1.2, 0); err != nil {
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

// buildLegacyDB creates a raw SQLite db with migrations 001..upto applied
// and an EMPTY schema_version table — the exact state of every db built in
// the buggy era before version tracking worked.
func buildLegacyDB(t *testing.T, path string, upto int) *sql.DB {
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	dirs, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs[:upto] {
		data, err := migrationsFS.ReadFile("migrations/" + d.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(data)); err != nil {
			t.Fatalf("apply %s: %v", d.Name(), err)
		}
	}
	if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	return db
}

// seedLegacyPricedRow inserts one leader markout row that was PRICED before
// statuses existed (observed_price set, status 'pending' where the column
// exists — the exact trap 007 must fix).
func seedLegacyPricedRow(t *testing.T, db *sql.DB, withStatus bool) {
	now := time.Now().UTC().Unix()
	if _, err := db.Exec(`INSERT INTO trade_events
		(event_id, source, chain, tx_hash, wallet, wallet_type, token_address, token_symbol,
		 side, position_action, amount_usd, token_amount, price_usd, buy_cost_usd,
		 trade_time, received_at, processed_at, raw_json)
		VALUES ('sol_upg_legacy', 'gmgn_smartmoney', 'sol', 'upg', 'W', 'smart_money', 'TOKEN_UP', 'UP',
		        'buy', NULL, 10, 1e18, 1.0, 10, ?, ?, ?, '{}')`, now-300, now-300, now); err != nil {
		t.Fatal(err)
	}
	cols := "event_id, kind, horizon_ms, base_ms, base_price, observed_price, return_pct, created_at"
	vals := "'sol_upg_legacy', 'leader', 30000, ?, 1.0, 1.1, 10, ?"
	args := []any{now - 300, now}
	if withStatus {
		cols += ", status"
		vals += ", 'pending'"
	}
	if _, err := db.Exec(`INSERT INTO markouts (`+cols+`) VALUES (`+vals+`)`, args...); err != nil {
		t.Fatal(err)
	}
}

// TestMigration007BackfillsFilled simulates the v0.1.3 → v0.1.3.1 upgrade:
// a legacy v6 db (no version row) whose priced rows are stuck on 'pending'
// must have 007 run — schema inferred to 6, not pinned to the latest — and
// the backfill must flip them to 'filled'.
func TestMigration007BackfillsFilled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.db")
	db := buildLegacyDB(t, path, 6) // v6 schema: status column exists
	seedLegacyPricedRow(t, db, true)
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var ver int
	if err := s.db.QueryRow(`SELECT version FROM schema_version`).Scan(&ver); err != nil {
		t.Fatal(err)
	}
	if ver != 9 {
		t.Errorf("legacy v6 db pinned to version %d, want 9 (007..009 must have run)", ver)
	}
	var st string
	if err := s.db.QueryRow(`SELECT status FROM markouts WHERE event_id = 'sol_upg_legacy'`).Scan(&st); err != nil {
		t.Fatal(err)
	}
	if st != MarkoutStatusFilled {
		t.Errorf("pre-v6 priced row status = %q after upgrade, want %q (007 backfill)", st, MarkoutStatusFilled)
	}
	// coverage must count it as filled
	filled, due, err := s.DueCoverage(MarkoutLeader, 30*time.Second, 0, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if filled != 1 || due != 1 {
		t.Errorf("coverage after upgrade = %d/%d, want 1/1", filled, due)
	}
}

// TestLegacyV5Inference upgrades a v5 db (no status column, no episodes
// table): inference must land on 5, then 006 creates the status column and
// episodes table, and 007 backfills the priced row.
func TestLegacyV5Inference(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v5.db")
	db := buildLegacyDB(t, path, 5)
	seedLegacyPricedRow(t, db, false) // v5 markouts has no status column
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var ver int
	if err := s.db.QueryRow(`SELECT version FROM schema_version`).Scan(&ver); err != nil {
		t.Fatal(err)
	}
	if ver != 9 {
		t.Errorf("legacy v5 db pinned to version %d, want 9", ver)
	}
	// 006 must have created the status column + episodes table
	if !hasColumn(s.db, "markouts", "status") {
		t.Error("markouts.status missing after v5→v8 upgrade")
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='position_episodes'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("position_episodes table missing after v5→v8 upgrade")
	}
	// 007 backfill: the priced row must be 'filled' (was pending by DEFAULT)
	var st string
	if err := s.db.QueryRow(`SELECT status FROM markouts WHERE event_id = 'sol_upg_legacy'`).Scan(&st); err != nil {
		t.Fatal(err)
	}
	if st != MarkoutStatusFilled {
		t.Errorf("v5-era priced row status = %q, want %q (007 backfill)", st, MarkoutStatusFilled)
	}
}

// TestReplicationCensus pins the actor-level survivor-bias guard: a wallet's
// census buckets EVERY due follower row (filled / market loss / measurement
// loss / unresolved) and carries the observed EV — a filled-only mean can
// no longer flatter an actor whose tokens mostly died.
func TestReplicationCensus(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	ts := now.Add(-20 * time.Minute)
	// Wallet W1: 4 due rows — 2 filled (+10%, +30%), 1 no_candle, 1 api_error
	// Wallet W2: 2 due rows — 1 filled (+5%), 1 pending (unresolved)
	mk := func(wallet, id string, ret *float64, status string) {
		ev := domain.TradeEvent{
			ID:     domain.EventID("sol", id, wallet, "T_"+id, "buy"),
			Source: "gmgn_smartmoney", Chain: "sol", TxHash: id,
			Wallet: wallet, WalletType: domain.WalletSmartMoney,
			TokenAddress: "T_" + id, Side: domain.Buy, AmountUSD: 10, PriceUSD: 1.0,
			TradeTime: ts, ReceivedAt: ts,
		}
		if _, err := s.InsertEvent(ev); err != nil {
			t.Fatal(err)
		}
		entry := 1.0
		if err := s.CreateMarkouts(ev, MarkoutFollower, &entry, []time.Duration{30 * time.Second}, now); err != nil {
			t.Fatal(err)
		}
		switch {
		case ret != nil:
			if err := s.FillMarkout(ev.ID, MarkoutFollower, 30*time.Second, 1.0+*ret/100, 0); err != nil {
				t.Fatal(err)
			}
		case status != "":
			if err := s.SetMarkoutStatus(ev.ID, MarkoutFollower, 30*time.Second, status); err != nil {
				t.Fatal(err)
			}
		}
	}
	p10, p30, p5 := 10.0, 30.0, 5.0
	mk("W1", "a", &p10, "")
	mk("W1", "b", &p30, "")
	mk("W1", "c", nil, MarkoutStatusNoCandle)
	mk("W1", "d", nil, MarkoutStatusAPIError)
	mk("W2", "e", &p5, "")
	mk("W2", "f", nil, "") // stays pending

	rows, err := s.ReplicationCensus(ts.Add(-time.Minute), 30*time.Second, 0, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	byWallet := map[string]ReplicationRow{}
	for _, r := range rows {
		byWallet[r.Wallet] = r
	}
	w1, ok := byWallet["W1"]
	if !ok {
		t.Fatalf("W1 missing from census: %v", byWallet)
	}
	if w1.Due != 4 || w1.Filled != 2 || w1.MarketLoss != 1 || w1.MeasLoss != 1 || w1.Unresolved != 0 {
		t.Errorf("W1 census = due%d filled%d mkt%d meas%d unres%d, want 4/2/1/1/0",
			w1.Due, w1.Filled, w1.MarketLoss, w1.MeasLoss, w1.Unresolved)
	}
	if !w1.ObservedValid || math.Abs(w1.ObservedEV-20) > 0.001 {
		t.Errorf("W1 observed EV = %v (valid %v), want +20", w1.ObservedEV, w1.ObservedValid)
	}
	w2 := byWallet["W2"]
	if w2.Due != 2 || w2.Filled != 1 || w2.Unresolved != 1 {
		t.Errorf("W2 census = due%d filled%d unres%d, want 2/1/1", w2.Due, w2.Filled, w2.Unresolved)
	}
	if !w2.ObservedValid || math.Abs(w2.ObservedEV-5) > 0.001 {
		t.Errorf("W2 observed EV = %v, want +5", w2.ObservedEV)
	}
}

// TestReplicationCensusWindowAndSide pins both P0 fixes: the census must
// describe the SAME window as Quality (--since) and only BUY entries —
// a sell leg's markout has the opposite sign and must not enter entry
// replication.
func TestReplicationCensusWindowAndSide(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	since := now.Add(-24 * time.Hour)
	mk := func(wallet, id string, side domain.Side, ts time.Time, ret float64) {
		ev := domain.TradeEvent{
			ID:     domain.EventID("sol", id, wallet, "T_"+id, string(side)),
			Source: "gmgn_smartmoney", Chain: "sol", TxHash: id,
			Wallet: wallet, WalletType: domain.WalletSmartMoney,
			TokenAddress: "T_" + id, Side: side, AmountUSD: 10, PriceUSD: 1.0,
			TradeTime: ts, ReceivedAt: ts,
		}
		if _, err := s.InsertEvent(ev); err != nil {
			t.Fatal(err)
		}
		entry := 1.0
		if err := s.CreateMarkouts(ev, MarkoutFollower, &entry, []time.Duration{30 * time.Second}, now); err != nil {
			t.Fatal(err)
		}
		if err := s.FillMarkout(ev.ID, MarkoutFollower, 30*time.Second, 1.0+ret/100, 0); err != nil {
			t.Fatal(err)
		}
	}
	mk("W_IN", "in1", domain.Buy, now.Add(-20*time.Minute), 10)       // in window, buy → included
	mk("W_OLD", "old1", domain.Buy, now.Add(-48*time.Hour), 100)      // buy but BEFORE since → excluded
	mk("W_SELL", "sell1", domain.Sell, now.Add(-20*time.Minute), 200) // huge sell return → excluded

	rows, err := s.ReplicationCensus(since, 30*time.Second, 0, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Wallet != "W_IN" {
		t.Fatalf("census = %+v, want only W_IN (window + buy side)", rows)
	}
	r := rows[0]
	if r.Due != 1 || r.Filled != 1 || !r.ObservedValid || math.Abs(r.ObservedEV-10) > 0.001 {
		t.Errorf("W_IN census = due%d filled%d ev%v, want 1/1/+10", r.Due, r.Filled, r.ObservedEV)
	}
}

// TestDueMarkoutsFreshFirst pins the fresh-first ordering: pending rows come
// before retry rows in the queue, and tokens in backoff are excluded at the
// SQL level so they can't occupy LIMIT slots and starve fresh data.
func TestDueMarkoutsFreshFirst(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	mk := func(id, token string, status string, ago time.Duration) {
		base := now.Add(-ago).Truncate(30 * time.Second)
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
		if err := s.CreateMarkouts(ev, MarkoutLeader, &price, []time.Duration{30 * time.Second}, now); err != nil {
			t.Fatal(err)
		}
		if err := s.SetMarkoutStatus(ev.ID, MarkoutLeader, 30*time.Second, status); err != nil {
			t.Fatal(err)
		}
	}
	mk("r1", "TOKEN_R", MarkoutStatusLookbackMiss, 10*time.Minute) // retry, newer
	mk("p1", "TOKEN_P", MarkoutStatusPending, 20*time.Minute)      // pending, older
	mk("b1", "TOKEN_B", MarkoutStatusNoKlineData, 30*time.Minute)  // in backoff

	// fresh pending first, retry after
	due, err := s.DueMarkouts(0, now, 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 3 || due[0].Token != "TOKEN_P" {
		t.Errorf("fresh-first order broken: %+v", due)
	}
	// backoff token excluded at SQL level
	due, err = s.DueMarkouts(0, now, 100, []string{"TOKEN_B"})
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 2 {
		t.Fatalf("skipTokens not honored: %+v", due)
	}
	for _, d := range due {
		if d.Token == "TOKEN_B" {
			t.Errorf("backoff token TOKEN_B leaked into queue: %+v", due)
		}
	}
}

// TestBehaviorQueries pins the v0.2.0.1 behavior queries: on-demand episode
// reconstruction (with behavior fields), prior flow at entry (PIT, strict <),
// and entry chase observations (never future-conditioned).
func TestBehaviorQueries(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	base := now.Add(-1 * time.Hour).Truncate(30 * time.Second)
	mk := func(id, side string, ts time.Time, tokens, usd, buyCost float64) {
		ev := domain.TradeEvent{
			ID:     domain.EventID("sol", id, "W_B", "TOKEN_X", side),
			Source: "gmgn_smartmoney", Chain: "sol", TxHash: id,
			Wallet: "W_B", WalletType: domain.WalletSmartMoney,
			TokenAddress: "TOKEN_X", Side: domain.Side(side), AmountUSD: usd,
			TokenAmount: tokens, PriceUSD: 1.0, BuyCostUSD: buyCost,
			TradeTime: ts, ReceivedAt: ts.Add(20 * time.Second),
		}
		if _, err := s.InsertEvent(ev); err != nil {
			t.Fatal(err)
		}
	}
	mk("b1", "buy", base, 100, 100, 0)                       // open
	mk("s1", "sell", base.Add(60*time.Second), 60, 70, 60)   // partial exit: qty 40 left
	mk("b2", "buy", base.Add(120*time.Second), 50, 50, 0)    // add: qty 90
	mk("s2", "sell", base.Add(180*time.Second), 90, 130, 90) // close at +180s

	// on-demand reconstruction — does NOT depend on the materialized table
	eps, err := s.ReconstructEpisodesFor("W_B", now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 {
		t.Fatalf("episodes = %+v, want 1", eps)
	}
	e := eps[0]
	if e.Status != EpisodeClosed || e.HoldDurationS != 180 {
		t.Errorf("episode = status %s hold %d, want closed/180", e.Status, e.HoldDurationS)
	}
	if e.Adds != 1 || e.Reduces != 2 || e.RealizedPnL != 50 {
		t.Errorf("episode = adds %d reduces %d pnl %.0f, want 1/2/50", e.Adds, e.Reduces, e.RealizedPnL)
	}
	// behavior fields: s1 left qty>0 → a REAL partial exit leg
	if e.PartialExitLegs != 1 || e.SellLegs != 2 || e.FirstSellAt != base.Unix()+60 {
		t.Errorf("behavior fields = partialLegs %d sellLegs %d firstSell %d, want 1/2/%d",
			e.PartialExitLegs, e.SellLegs, e.FirstSellAt, base.Unix()+60)
	}
	if e.InitialBuyUSD != 100 || e.AddBuyUSD != 50 || e.DataGap {
		t.Errorf("behavior fields = init %.0f add %.0f gap %v, want 100/50/false",
			e.InitialBuyUSD, e.AddBuyUSD, e.DataGap)
	}

	// data-gap episode: a sell with no visible position is gap, not partial exit
	if _, err := s.InsertEvent(domain.TradeEvent{
		ID:     domain.EventID("sol", "gap1", "W_B", "TOKEN_G", "sell"),
		Source: "gmgn_smartmoney", Chain: "sol", TxHash: "gap1",
		Wallet: "W_B", WalletType: domain.WalletSmartMoney,
		TokenAddress: "TOKEN_G", Side: domain.Sell, AmountUSD: 10,
		TokenAmount: 5, PriceUSD: 1.0, BuyCostUSD: 3,
		TradeTime: base.Add(10 * time.Minute), ReceivedAt: base.Add(10*time.Minute + 20*time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	eps, err = s.ReconstructEpisodesFor("W_B", now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var gap *Episode
	for i := range eps {
		if eps[i].Token == "TOKEN_G" {
			gap = &eps[i]
		}
	}
	if gap == nil || !gap.DataGap || gap.PartialExitLegs != 0 || gap.Status != EpisodePartial {
		t.Errorf("gap episode = %+v, want DataGap=true PartialExitLegs=0 status=partial", gap)
	}

	// prior flow at entry: strict [T-window, T) — same-second trades excluded
	if _, err := s.InsertEvent(domain.TradeEvent{
		ID:     domain.EventID("sol", "prior1", "OTHER_SMART", "TOKEN_X", "buy"),
		Source: "gmgn_smartmoney", Chain: "sol", TxHash: "prior1",
		Wallet: "OTHER_SMART", WalletType: domain.WalletSmartMoney,
		TokenAddress: "TOKEN_X", Side: domain.Buy, AmountUSD: 10,
		TokenAmount: 10, PriceUSD: 1.0, BuyCostUSD: 0,
		TradeTime: base.Add(-55 * time.Second), ReceivedAt: base.Add(-55*time.Second + 20*time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	// filler: pushes dataset start before the 60s prior-flow window (base-70s)
	if _, err := s.InsertEvent(domain.TradeEvent{
		ID:     domain.EventID("sol", "filler", "FILLER", "TOKEN_X", "buy"),
		Source: "gmgn_smartmoney", Chain: "sol", TxHash: "filler",
		Wallet: "FILLER", WalletType: domain.WalletSmartMoney,
		TokenAddress: "TOKEN_X", Side: domain.Buy, AmountUSD: 10,
		TokenAmount: 10, PriceUSD: 1.0, BuyCostUSD: 0,
		TradeTime: base.Add(-70 * time.Second), ReceivedAt: base.Add(-70*time.Second + 20*time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	// same-second buy (excluded conservatively)
	if _, err := s.InsertEvent(domain.TradeEvent{
		ID:     domain.EventID("sol", "prior2", "SAME_SEC", "TOKEN_X", "buy"),
		Source: "gmgn_smartmoney", Chain: "sol", TxHash: "prior2",
		Wallet: "SAME_SEC", WalletType: domain.WalletSmartMoney,
		TokenAddress: "TOKEN_X", Side: domain.Buy, AmountUSD: 10,
		TokenAmount: 10, PriceUSD: 1.0, BuyCostUSD: 0,
		TradeTime: base, ReceivedAt: base.Add(20 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	ds, err := s.DatasetStart()
	if err != nil {
		t.Fatal(err)
	}
	pf, err := s.PriorFlowAt("TOKEN_X", base.Unix(), time.Minute, ds)
	if err != nil {
		t.Fatal(err)
	}
	if !pf.Valid || pf.Smart != 1 || pf.KOL != 0 {
		t.Errorf("prior flow = %+v, want valid smart=1 kol=0 (same-second buy must be excluded)", pf)
	}
	// window before the dataset → invalid, NOT fabricated zero
	pf, err = s.PriorFlowAt("TOKEN_X", ds+30, time.Hour, ds)
	if err != nil {
		t.Fatal(err)
	}
	if pf.Valid {
		t.Errorf("prior flow at dataset edge must be invalid, got %+v", pf)
	}

	// entry chase observations: base_price set, NO horizon fill required
	ev := domain.TradeEvent{
		ID:     domain.EventID("sol", "ch1", "W_B", "TOKEN_CH", "buy"),
		Source: "gmgn_smartmoney", Chain: "sol", TxHash: "ch1",
		Wallet: "W_B", WalletType: domain.WalletSmartMoney,
		TokenAddress: "TOKEN_CH", Side: domain.Buy, AmountUSD: 10,
		TokenAmount: 10, PriceUSD: 1.0, BuyCostUSD: 0,
		TradeTime: base, ReceivedAt: base,
	}
	if _, err := s.InsertEvent(ev); err != nil {
		t.Fatal(err)
	}
	entry := 1.1 // follower entry 10% above leader price → chase +10%
	if err := s.CreateMarkouts(ev, MarkoutFollower, &entry, []time.Duration{30 * time.Second}, now); err != nil {
		t.Fatal(err)
	}
	// NO FillMarkout: the 5m outcome is missing; chase must still be observable
	obs, err := s.EntryObservations("W_B", now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, o := range obs {
		if o.EventID == ev.ID {
			found = true
			if math.Abs(o.ChasePct-10) > 0.001 {
				t.Errorf("chase = %.2f%%, want +10%%", o.ChasePct)
			}
		}
	}
	if !found {
		t.Errorf("entry observation missing for unfilled horizon — chase must not depend on outcome: %+v", obs)
	}
}

// TestMigration009BackfillsEventEntry pins the P0 fix: pre-SetFollowerEntry
// databases have events where only the short horizon captured the follower
// entry and longer horizons sit entryless in lookback_miss. Migration 009
// must propagate the known entry to every row of the event; the rows keep
// their retryable status so the engine fills the outcome on the next pass.
func TestMigration009BackfillsEventEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v8.db")
	db := buildLegacyDB(t, path, 8) // 001-008 applied, empty schema_version
	now := time.Now().UTC().Unix()
	if _, err := db.Exec(`INSERT INTO trade_events
		(event_id, source, chain, tx_hash, wallet, wallet_type, token_address, token_symbol,
		 side, position_action, amount_usd, token_amount, price_usd, buy_cost_usd,
		 trade_time, received_at, processed_at, raw_json)
		VALUES ('sol_legacy_entry', 'gmgn_smartmoney', 'sol', 'leg', 'W', 'smart_money', 'TOKEN_LE', 'LE',
		        'buy', NULL, 10, 1e18, 1.0, 10, ?, ?, ?, '{}')`, now-3600, now-3600, now); err != nil {
		t.Fatal(err)
	}
	// 30s row: entry captured + filled. 5m row: entryless lookback_miss —
	// exactly the pre-fix state.
	if _, err := db.Exec(`INSERT INTO markouts
		(event_id, kind, horizon_ms, base_ms, base_price, observed_price, return_pct,
		 created_at, status, entry_observed_at)
		VALUES ('sol_legacy_entry', 'follower', 30000, ?, 1.0, 1.1, 10, ?, 'filled', ?),
		       ('sol_legacy_entry', 'follower', 300000, ?, NULL, NULL, NULL, ?, 'lookback_miss', NULL)`,
		now-3600, now, now-3600, now-3600, now); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var bp float64
	var eoa, wantEoa int64
	var status string
	if err := s.db.QueryRow(`SELECT base_price, entry_observed_at, status FROM markouts
		WHERE event_id = 'sol_legacy_entry' AND horizon_ms = 300000`).Scan(&bp, &eoa, &status); err != nil {
		t.Fatal(err)
	}
	wantEoa = now - 3600
	if bp != 1.0 || eoa != wantEoa {
		t.Errorf("5m row after 009 = base %.2f observed %d, want 1.00/%d (entry propagated from 30s row)", bp, eoa, wantEoa)
	}
	if status != MarkoutStatusLookbackMiss {
		t.Errorf("5m row status = %s, want lookback_miss (must stay retryable so the outcome fills)", status)
	}
	// the row is now due-eligible and the engine can fill it without entry lookup
	due, err := s.DueMarkouts(0, time.Now().UTC(), 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range due {
		if d.EventID == "sol_legacy_entry" && d.Horizon == 5*time.Minute {
			found = true
			if d.BasePrice != 1.0 {
				t.Errorf("due row base_price = %.2f, want 1.0 (backfilled entry must be visible to the engine)", d.BasePrice)
			}
		}
	}
	if !found {
		t.Errorf("5m row must be due-eligible after backfill: %+v", due)
	}
}

// TestPositionBookEpsilon pins the shared-accounting fix: episode
// reconstruction and entry classification must agree on position state even
// with float64 residue on large quantities — a sum of three 333.3333 buys
// minus a 1000 sell is a FULL CLOSE, not a data gap, and the next buy opens
// a new episode (initial), not an add.
func TestPositionBookEpsilon(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	base := now.Add(-1 * time.Hour).Truncate(30 * time.Second)
	mk := func(id, side string, ts time.Time, qty float64) {
		ev := domain.TradeEvent{
			ID:     domain.EventID("sol", id, "W_E", "TOKEN_E", side),
			Source: "gmgn_smartmoney", Chain: "sol", TxHash: id,
			Wallet: "W_E", WalletType: domain.WalletSmartMoney,
			TokenAddress: "TOKEN_E", Side: domain.Side(side), AmountUSD: qty,
			TokenAmount: qty, PriceUSD: 1.0, BuyCostUSD: 0,
			TradeTime: ts, ReceivedAt: ts,
		}
		if _, err := s.InsertEvent(ev); err != nil {
			t.Fatal(err)
		}
	}
	// 3 buys of 333.3333333333333 accumulate to 999.9999999999999;
	// the 1000 sell leaves ~-1.1e-13 residue — far inside the relative epsilon.
	mk("b1", "buy", base, 333.3333333333333)
	mk("b2", "buy", base.Add(30*time.Second), 333.3333333333333)
	mk("b3", "buy", base.Add(60*time.Second), 333.3333333333333)
	mk("s1", "sell", base.Add(90*time.Second), 1000.0)
	mk("b4", "buy", base.Add(120*time.Second), 50.0) // must be a NEW episode's opening buy

	eps, err := s.ReconstructEpisodesFor("W_E", now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 2 {
		t.Fatalf("episodes = %+v, want 2 (full close + reopen)", eps)
	}
	if eps[0].Status != EpisodeClosed || eps[0].Adds != 2 || eps[0].DataGap {
		t.Errorf("episode1 = status %s adds %d gap %v, want closed/2/false (residue must not be a data gap)", eps[0].Status, eps[0].Adds, eps[0].DataGap)
	}
	if eps[1].InitialBuyUSD != 50 || eps[1].Adds != 0 {
		t.Errorf("episode2 = init %.0f adds %d, want 50/0 (reopened, not an add)", eps[1].InitialBuyUSD, eps[1].Adds)
	}

	// classification agrees: b4 is INITIAL, not an add
	classified, err := s.ClassifiedEntries("W_E")
	if err != nil {
		t.Fatal(err)
	}
	var initials, adds int
	for _, ce := range classified {
		if ce.Initial {
			initials++
		} else {
			adds++
		}
	}
	if initials != 2 || adds != 2 { // b1, b4 initial; b2, b3 adds
		t.Errorf("classification = initial %d adds %d, want 2/2 (shared epsilon)", initials, adds)
	}
}

// TestPositionBookPeakReset pins the P1-high lifecycle fix: a full close
// must reset the book's PEAK. Otherwise the next episode inherits a huge
// epsilon (1e12 peak → eps 1000), a 10-token opening buy looks empty, and
// the following 5-token buy is misclassified as initial instead of add.
func TestPositionBookPeakReset(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	base := now.Add(-1 * time.Hour).Truncate(30 * time.Second)
	mk := func(id, side string, ts time.Time, qty float64) {
		ev := domain.TradeEvent{
			ID:     domain.EventID("sol", id, "W_PK", "TOKEN_PK", side),
			Source: "gmgn_smartmoney", Chain: "sol", TxHash: id,
			Wallet: "W_PK", WalletType: domain.WalletSmartMoney,
			TokenAddress: "TOKEN_PK", Side: domain.Side(side), AmountUSD: qty,
			TokenAmount: qty, PriceUSD: 1.0, BuyCostUSD: 0,
			TradeTime: ts, ReceivedAt: ts,
		}
		if _, err := s.InsertEvent(ev); err != nil {
			t.Fatal(err)
		}
	}
	mk("b1", "buy", base, 1e12)                      // huge episode
	mk("s1", "sell", base.Add(30*time.Second), 1e12) // full close (peak must reset)
	mk("b2", "buy", base.Add(60*time.Second), 10)    // new episode opening → initial
	mk("b3", "buy", base.Add(90*time.Second), 5)     // MUST be an add

	classified, err := s.ClassifiedEntries("W_PK")
	if err != nil {
		t.Fatal(err)
	}
	var initials, adds int
	for _, ce := range classified {
		if ce.Initial {
			initials++
		} else {
			adds++
		}
	}
	if initials != 2 || adds != 1 { // b1, b2 initial; b3 add
		t.Errorf("classification = initial %d add %d, want 2/1 (b3 must be an add — leaked peak would say initial)", initials, adds)
	}

	eps, err := s.ReconstructEpisodesFor("W_PK", now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 2 || eps[1].Adds != 1 {
		t.Errorf("episodes = %+v, want 2 episodes with the second having 1 add (b3)", eps)
	}
}

// TestOriginKnownLifecycle pins the P0.5 left-censoring rule: the first
// observed episode of a (wallet, token) is not provably an initial entry
// (collector may have missed an earlier position); after a CONFIRMED full
// close, later opening buys are origin-known.
func TestOriginKnownLifecycle(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	base := now.Add(-1 * time.Hour).Truncate(30 * time.Second)
	mk := func(id, token, side string, ts time.Time, qty float64) {
		ev := domain.TradeEvent{
			ID:     domain.EventID("sol", id, "W_OR", token, side),
			Source: "gmgn_smartmoney", Chain: "sol", TxHash: id,
			Wallet: "W_OR", WalletType: domain.WalletSmartMoney,
			TokenAddress: token, Side: domain.Side(side), AmountUSD: qty,
			TokenAmount: qty, PriceUSD: 1.0, BuyCostUSD: 0,
			TradeTime: ts, ReceivedAt: ts,
		}
		if _, err := s.InsertEvent(ev); err != nil {
			t.Fatal(err)
		}
	}
	// TOKEN_A: first observed buy is left-censored, even though it looks
	// like a clean opening (no sell before it). After a full close, the
	// second episode's opening buy is origin-known.
	mk("a1", "TOKEN_A", "buy", base, 100)
	mk("a2", "TOKEN_A", "sell", base.Add(30*time.Second), 100) // confirmed full close
	mk("a3", "TOKEN_A", "buy", base.Add(60*time.Second), 50)   // origin-known opening
	// TOKEN_B: never closes — its single opening buy stays censored.
	mk("b1", "TOKEN_B", "buy", base, 200)

	eps, err := s.ReconstructEpisodesFor("W_OR", now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	byToken := map[string][]Episode{}
	for _, e := range eps {
		byToken[e.Token] = append(byToken[e.Token], e)
	}
	a := byToken["TOKEN_A"]
	if len(a) != 2 {
		t.Fatalf("TOKEN_A episodes = %+v, want 2", a)
	}
	if a[0].OriginKnown {
		t.Errorf("first observed episode must be left-censored (OriginKnown=false): %+v", a[0])
	}
	if !a[1].OriginKnown {
		t.Errorf("post-full-close episode must be origin-known: %+v", a[1])
	}
	b := byToken["TOKEN_B"]
	if len(b) != 1 || b[0].OriginKnown {
		t.Errorf("never-closed token's episode must stay censored: %+v", b)
	}

	// classification agrees
	classified, err := s.ClassifiedEntries("W_OR")
	if err != nil {
		t.Fatal(err)
	}
	originByID := map[string]bool{}
	for _, ce := range classified {
		originByID[ce.EventID] = ce.OriginKnown
	}
	a1 := domain.EventID("sol", "a1", "W_OR", "TOKEN_A", "buy")
	a3 := domain.EventID("sol", "a3", "W_OR", "TOKEN_A", "buy")
	if originByID[a1] {
		t.Errorf("a1 (first observed buy) must be censored in classification too")
	}
	if !originByID[a3] {
		t.Errorf("a3 (post-close opening) must be origin-known in classification")
	}
}
