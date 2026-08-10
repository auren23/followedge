-- v2: actor-centric foundation.
-- buy_cost_usd (present on sells in the GMGN feed) enables on-chain realized
-- PnL reconstruction without any extra API calls.

ALTER TABLE trade_events ADD COLUMN buy_cost_usd REAL;

CREATE TABLE actors (
    id INTEGER PRIMARY KEY AUTOINCREMENT,

    address TEXT NOT NULL UNIQUE,
    actor_type TEXT NOT NULL,      -- feed label: smart_money | kol
    chain TEXT NOT NULL,

    first_seen_at INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,

    trade_count INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_actors_last_seen ON actors(last_seen_at);
