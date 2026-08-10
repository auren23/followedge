// Package storage persists normalized events, cluster samples and markouts
// in SQLite (WAL). SQLite is the entire backplane for v0.1; a columnar store
// only becomes relevant at multi-million-event scale.
package storage

import (
	"database/sql"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no cgo

	"github.com/auren23/followedge/internal/domain"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store is a thin, safe wrapper over the SQLite handle.
type Store struct {
	db *sql.DB
}

// EventRow is the persisted shape of a trade event (timestamps in unix secs).
type EventRow struct {
	ID             string
	Source         string
	Chain          string
	TxHash         string
	Wallet         string
	WalletType     string
	TokenAddress   string
	TokenSymbol    string
	Side           string
	PositionAction string
	AmountUSD      float64
	TokenAmount    float64
	PriceUSD       float64
	TradeTime      int64
	ReceivedAt     int64
	RawJSON        string
}

func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // sqlite: one writer, avoid SQLITE_BUSY churn
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// DB exposes the raw handle for tests and diagnostics.
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate() error {
	if _, err := s.db.Exec("CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)"); err != nil {
		return err
	}

	// migrations/*.sql in lexical order; each file's version is its numeric
	// prefix (001, 002, ...) and it runs exactly once per database.
	dirs, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(dirs))
	maxVer := 0
	for _, d := range dirs {
		names = append(names, d.Name())
		if ver, err := strconv.Atoi(d.Name()[:3]); err == nil && ver > maxVer {
			maxVer = ver
		}
	}
	sort.Strings(names)

	var cur int
	err = s.db.QueryRow("SELECT version FROM schema_version").Scan(&cur)
	if err == sql.ErrNoRows {
		// no version row: seed it. Without a row every UPDATE below affects
		// 0 rows and the whole set re-runs on every Open, crashing on
		// "table already exists" the second time a db is opened.
		// A pre-existing db (trade_events present) was built in that buggy
		// era and already has every migration applied — pin it to maxVer.
		var n int
		if err := s.db.QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='trade_events'").Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			cur = maxVer
		} else {
			cur = 0
		}
		if _, err := s.db.Exec("INSERT INTO schema_version (version) VALUES (?)", cur); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	for _, name := range names {
		ver, err := strconv.Atoi(name[:3])
		if err != nil {
			return fmt.Errorf("bad migration name %q", name)
		}
		if ver <= cur {
			continue
		}
		data, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		if _, err := s.db.Exec(string(data)); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := s.db.Exec("UPDATE schema_version SET version = ?", ver); err != nil {
			return err
		}
	}
	return nil
}

// InsertEvent stores an event. Returns created=false when the event_id was
// already present (duplicate poll, restart replay) — only freshly created
// events may flow into the cluster pipeline.
func (s *Store) InsertEvent(e domain.TradeEvent) (created bool, err error) {
	res, err := s.db.Exec(`
		INSERT OR IGNORE INTO trade_events
		(event_id, source, chain, tx_hash, wallet, wallet_type, token_address, token_symbol,
		 side, position_action, amount_usd, token_amount, price_usd, buy_cost_usd,
		 trade_time, received_at, processed_at, raw_json)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.ID, e.Source, e.Chain, e.TxHash, e.Wallet, string(e.WalletType), e.TokenAddress, e.TokenSymbol,
		string(e.Side), string(e.PositionAction), e.AmountUSD, e.TokenAmount, e.PriceUSD, e.BuyCostUSD,
		e.TradeTime.Unix(), e.ReceivedAt.Unix(), e.ProcessedAt.Unix(), e.RawJSON)
	if err != nil {
		return false, fmt.Errorf("insert event: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Store) EventCount() (int64, error) {
	var n int64
	err := s.db.QueryRow("SELECT COUNT(*) FROM trade_events").Scan(&n)
	return n, err
}

func (s *Store) Counts() (events, markouts, clusters int64, err error) {
	err = s.db.QueryRow("SELECT COUNT(*) FROM trade_events").Scan(&events)
	if err != nil {
		return
	}
	err = s.db.QueryRow("SELECT COUNT(*) FROM markouts").Scan(&markouts)
	if err != nil {
		return
	}
	err = s.db.QueryRow("SELECT COUNT(*) FROM cluster_samples").Scan(&clusters)
	return
}

// RecentEvents returns the last `since` worth of events for a token, newest
// first — the input for rolling-window cluster features.
func (s *Store) RecentEvents(token string, since time.Time) ([]EventRow, error) {
	rows, err := s.db.Query(`
		SELECT event_id, source, wallet, wallet_type, side, amount_usd, trade_time
		FROM trade_events
		WHERE token_address = ? AND trade_time >= ?
		ORDER BY trade_time DESC`, token, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventRow
	for rows.Next() {
		var r EventRow
		if err := rows.Scan(&r.ID, &r.Source, &r.Wallet, &r.WalletType, &r.Side, &r.AmountUSD, &r.TradeTime); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LatencyRow is the minimal shape needed for source-age analysis.
type LatencyRow struct {
	WalletType string
	TradeTime  int64
	ReceivedAt int64
}

// RecentEventsAll returns events received since `since` across all tokens.
func (s *Store) RecentEventsAll(since time.Time) ([]LatencyRow, error) {
	rows, err := s.db.Query(`
		SELECT wallet_type, trade_time, received_at
		FROM trade_events
		WHERE received_at >= ?
		ORDER BY received_at`, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LatencyRow
	for rows.Next() {
		var r LatencyRow
		if err := rows.Scan(&r.WalletType, &r.TradeTime, &r.ReceivedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
