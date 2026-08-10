-- v3 (measurement-fix): split leader vs follower markouts.
--
-- v0.1.0 conflated two different questions in one table:
--   "did the leader's trade pay off"  (base = TradeTime)
--   "would a follower at OUR latency pay off"  (base = ReceivedAt)
-- Replicability/chase analysis silently used the leader number, which is
-- wrong: GMGN REST median source age is ~140s, so leader EV says nothing
-- about follower EV.
--
-- New shape:
--   kind      'leader'   base_ms = trade_time,   base_price = leader's price_usd
--             'follower' base_ms = received_at,  base_price = price at ReceivedAt
--                         (sampled later; NULL until then)
--   return_pct  (observed/base - 1) * 100 — same formula, different question
--               per kind. Analysis MUST filter by kind.

CREATE TABLE markouts_v2 (
    id INTEGER PRIMARY KEY AUTOINCREMENT,

    event_id TEXT NOT NULL,
    kind TEXT NOT NULL DEFAULT 'leader',

    horizon_ms INTEGER NOT NULL,

    -- sampling base: unix seconds (trade_time for leader, received_at for follower)
    base_ms INTEGER NOT NULL,

    base_price REAL,        -- NULL until entry price is sampled (follower)
    observed_price REAL,    -- NULL until horizon price is sampled
    return_pct REAL,

    created_at INTEGER NOT NULL,

    UNIQUE(event_id, kind, horizon_ms)
);

-- migrate existing rows as leader markouts, backfilling base_ms from the
-- event's trade_time (COALESCE guards against orphan rows)
INSERT INTO markouts_v2 (id, event_id, kind, horizon_ms, base_ms, base_price, observed_price, return_pct, created_at)
SELECT m.id, m.event_id, 'leader', m.horizon_ms,
       COALESCE(e.trade_time, m.created_at), m.base_price, m.observed_price, m.return_pct, m.created_at
FROM markouts m LEFT JOIN trade_events e ON e.event_id = m.event_id;

DROP TABLE markouts;
ALTER TABLE markouts_v2 RENAME TO markouts;

CREATE INDEX idx_markouts_due ON markouts(kind, base_ms, horizon_ms) WHERE observed_price IS NULL;
CREATE INDEX idx_markouts_event ON markouts(event_id, kind);
