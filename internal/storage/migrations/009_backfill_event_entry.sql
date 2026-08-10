-- v9: backfill follower entry to every horizon row of an event (v0.2.0.3).
--
-- Before SetFollowerEntry, the first horizon to come due captured the
-- follower entry and later horizons went lookback_miss when their kline
-- window no longer reached the entry candle. Propagate the earliest known
-- entry to every entryless row of the same event.
--
-- Backfilled rows keep their status (e.g. lookback_miss is retryable):
-- DueMarkouts re-lists them and the engine now finds base_price set, so it
-- skips the entry lookup and fills the horizon outcome directly.

UPDATE markouts AS dst
SET
    base_price = (
        SELECT src.base_price FROM markouts src
        WHERE src.event_id = dst.event_id AND src.kind = 'follower'
          AND src.base_price IS NOT NULL
        ORDER BY src.entry_observed_at
        LIMIT 1
    ),
    entry_observed_at = (
        SELECT src.entry_observed_at FROM markouts src
        WHERE src.event_id = dst.event_id AND src.kind = 'follower'
          AND src.base_price IS NOT NULL
        ORDER BY src.entry_observed_at
        LIMIT 1
    )
WHERE dst.kind = 'follower'
  AND dst.base_price IS NULL
  AND EXISTS (
      SELECT 1 FROM markouts src
      WHERE src.event_id = dst.event_id AND src.kind = 'follower'
        AND src.base_price IS NOT NULL
  );
