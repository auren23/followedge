-- v5: profit evidence levels. An actor can hold multiple evidence rows; the
-- best level is what a research card shows. E1 = GMGN feed derived, E3 =
-- chain-reconstructed realized PnL (v0.2+, requires own tx parsing).

CREATE TABLE actor_evidence (
    id INTEGER PRIMARY KEY AUTOINCREMENT,

    actor_id TEXT NOT NULL,
    evidence_type TEXT NOT NULL,   -- 'realized_pnl'
    level TEXT NOT NULL,           -- E0..E4
    source TEXT NOT NULL,          -- 'gmgn_feed'

    period_start INTEGER NOT NULL, -- unix seconds, inclusive
    period_end INTEGER NOT NULL,

    trade_count INTEGER NOT NULL,
    realized_pnl REAL,

    metadata_json TEXT,
    verified_at INTEGER NOT NULL,

    UNIQUE(actor_id, evidence_type, level, period_start, period_end)
);

CREATE INDEX idx_actor_evidence_actor ON actor_evidence(actor_id);
