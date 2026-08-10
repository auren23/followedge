-- v8: outcome observation instant + stale-outcome status (v0.1.3.2).
--
-- outcome_observed_at = candle close instant (open + resolution) of the
-- candle whose close was used as the horizon price. Allows staleness
-- analysis: is a fill really the fixed-horizon outcome, or the next trade
-- after a gap?
--
-- stale_outcome: the token's candle stream continued past the horizon, but
-- the first candle at/after due opens later than due + resolution — there
-- was no executable price at the fixed horizon. A MARKET outcome (like
-- no_candle), not a measurement failure.

ALTER TABLE markouts ADD COLUMN outcome_observed_at INTEGER;
