package storage

import (
	"database/sql"
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
	if ver != 8 {
		t.Errorf("legacy v6 db pinned to version %d, want 8 (007+008 must have run)", ver)
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
	if ver != 8 {
		t.Errorf("legacy v5 db pinned to version %d, want 8", ver)
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
