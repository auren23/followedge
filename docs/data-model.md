# Data Model

Canonical DDL lives in `internal/storage/migrations/*.sql` (versioned by
numeric prefix; each file runs exactly once per database). This doc explains
intent.

## trade_events

The most important table. One row per normalized smart-money/KOL trade.

- `event_id` — `sha256(chain|tx_hash|wallet|token|side)[:16]`, `UNIQUE`.
  Deterministic across restarts; dedup gate.
- `buy_cost_usd` — the seller's original buy cost (GMGN feed provides it on
  sell legs). `RealizedPnL = amount_usd − buy_cost_usd` per sell: on-chain
  profit verification with zero extra API calls.
- `raw_json` — untouched upstream payload; schema changes never destroy
  history (re-normalize on demand).
- `wallet_type` — `smart_money` | `kol` (from the GMGN feed, not inferred).
- `position_action` — `open` | `reduce` from `is_open_or_close`
  (0 → open/add, 1 → close/reduce; GMGN smartmoney/kol semantics).
- timestamps: unix seconds UTC.

Indexes: `(token_address, trade_time)` for cluster windows,
`(trade_time)` for latency analysis, `(wallet, trade_time)` for actor
aggregation.

## actors

The actor universe: one row per wallet ever seen, with first/last seen and a
running trade counter (maintained incrementally on insert). Rankings are
computed live from `trade_events` + `markouts` — this table is the index, not
the analytics.

## cluster_samples

Append-only rolling-window state. Every time an event lands, one row per
configured window (30s/60s/5m/15m) records the distinct-wallet convergence at
that moment. Counts are **distinct wallets**, never trade counts. Will feed
mechanism mining (e.g. "do copyable actors buy tokens where 2+ smart wallets
already converged?") in v0.2.

## markouts

Forward returns: for each event, one row per horizon
(30s/1m/3m/5m/15m/1h).

- `base_price` — leader's price (`price_usd`).
- `observed_price` — close of the first 30s candle opening at/after
  `TradeTime + horizon`; `NULL` until sampled (partial index makes "find
  due" cheap). Tokens that stopped trading before sampling stay `NULL`.
- `return_pct` — `(observed/base − 1) × 100`. This is the chase number: what
  a copy-trader entering `horizon` after the leader paid. Bucketed by
  `analyze chase`, averaged per actor at each horizon for alpha decay.

## Not yet present (v0.2+)

`token_snapshots`, `wallet_profiles` (enrichment — only justified once there
are candidates), `mechanism_hypotheses`, `experiments`, `strategy_registry`
(lineage: origin_actor/evidence/hypothesis/experiment), `paper_orders`,
`fills`, `positions`, `risk_limits` (execution layers).
