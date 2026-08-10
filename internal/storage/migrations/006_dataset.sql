-- v6: markout status + position episodes (v0.1.3-dataset).
--
-- markouts.status records WHY a row is not filled, so EV analysis can report
-- coverage and avoid selection bias (unpriced tokens are usually the worst
-- performers; excluding them inflates EV).
-- position_episodes reconstructs per-(wallet, token) trade sequences into
-- full position episodes (adds/reduces/capital/pnl/hold).

ALTER TABLE markouts ADD COLUMN status TEXT NOT NULL DEFAULT 'pending';

CREATE TABLE position_episodes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,

    wallet TEXT NOT NULL,
    token TEXT NOT NULL,

    opened_at INTEGER NOT NULL,   -- unix seconds
    closed_at INTEGER,            -- NULL while open

    adds INTEGER NOT NULL DEFAULT 0,
    reduces INTEGER NOT NULL DEFAULT 0,

    capital_in REAL NOT NULL DEFAULT 0,
    capital_out REAL NOT NULL DEFAULT 0,

    realized_pnl REAL NOT NULL DEFAULT 0,

    hold_duration_s INTEGER,      -- closed_at - opened_at

    -- 'open' | 'closed' | 'partial' (partial = sell legs exceeded the
    -- visible position, i.e. data gap before our collection window)
    status TEXT NOT NULL
);

CREATE INDEX idx_episodes_wallet ON position_episodes(wallet, closed_at);
CREATE INDEX idx_episodes_token ON position_episodes(token, opened_at);
