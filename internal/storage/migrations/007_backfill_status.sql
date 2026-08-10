-- v7: backfill markout status for rows created before v6 (v0.1.3.1).
--
-- v6 added `status DEFAULT 'pending'` without backfilling, so every row
-- that was ALREADY priced before the upgrade stayed 'pending' forever and
-- coverage numerators (filled / due) undercounted. Rows with a sampled
-- price were 'filled'; everything else genuinely had no outcome yet.

UPDATE markouts SET status = 'filled' WHERE observed_price IS NOT NULL AND status = 'pending';
