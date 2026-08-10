-- v4 (entry point-in-time): follower entry prices must not contain future
-- information. v0.1.1 sampled the entry as the close of the candle in
-- progress at ReceivedAt — but that candle closes up to 30s AFTER reception,
-- so the entry silently looked ahead.
--
-- Fix: entry = close of the last candle ALREADY CLOSED at ReceivedAt
-- (stale 0..30s, never look-ahead), and record the price's true time so
-- analysis can label it.
--
-- entry_observed_at: unix seconds — the instant the entry price actually
-- represents (candle open + resolution). NULL for leader rows (their base
-- price IS the trade price) and for unsampled follower rows.

ALTER TABLE markouts ADD COLUMN entry_observed_at INTEGER;
