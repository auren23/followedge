CREATE TABLE trade_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,

    event_id TEXT NOT NULL UNIQUE,

    source TEXT NOT NULL,
    chain TEXT NOT NULL,

    tx_hash TEXT NOT NULL,

    wallet TEXT NOT NULL,
    wallet_type TEXT NOT NULL,

    token_address TEXT NOT NULL,
    token_symbol TEXT,

    side TEXT NOT NULL,
    position_action TEXT,

    amount_usd REAL,
    token_amount REAL,
    price_usd REAL,

    -- unix seconds, UTC
    trade_time INTEGER NOT NULL,
    received_at INTEGER NOT NULL,
    processed_at INTEGER NOT NULL,

    raw_json TEXT NOT NULL
);

CREATE INDEX idx_trade_events_token_time ON trade_events(token_address, trade_time);
CREATE INDEX idx_trade_events_time ON trade_events(trade_time);
CREATE INDEX idx_trade_events_wallet ON trade_events(wallet, trade_time);

-- One row per (token, window) written every time an event lands: the rolling
-- cluster state at that moment, append-only so analysis keeps history.
CREATE TABLE cluster_samples (
    id INTEGER PRIMARY KEY AUTOINCREMENT,

    token_address TEXT NOT NULL,
    window_ms INTEGER NOT NULL,
    ts INTEGER NOT NULL,          -- last event time in window (unix secs)

    smart_buy_wallets INTEGER NOT NULL,
    smart_sell_wallets INTEGER NOT NULL,
    kol_buy_wallets INTEGER NOT NULL,
    kol_sell_wallets INTEGER NOT NULL,

    smart_buy_usd REAL NOT NULL DEFAULT 0,
    smart_sell_usd REAL NOT NULL DEFAULT 0,
    net_smart_flow_usd REAL NOT NULL DEFAULT 0,

    event_count INTEGER NOT NULL
);

CREATE INDEX idx_cluster_samples_token_ts ON cluster_samples(token_address, ts);
CREATE INDEX idx_cluster_samples_window ON cluster_samples(window_ms, ts);

-- Forward returns of each event at each horizon. observed_price stays NULL
-- until the markout worker samples it; partial index makes "find due" cheap.
CREATE TABLE markouts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,

    event_id TEXT NOT NULL,

    horizon_ms INTEGER NOT NULL,

    base_price REAL NOT NULL,
    observed_price REAL,
    return_pct REAL,

    created_at INTEGER NOT NULL,

    UNIQUE(event_id, horizon_ms)
);

CREATE INDEX idx_markouts_due ON markouts(horizon_ms, id) WHERE observed_price IS NULL;
CREATE INDEX idx_markouts_event ON markouts(event_id);
