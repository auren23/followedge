# FollowEdge Architecture

## Principles

1. **Actor-first, not signal-first.** The research object is *who made money
   and can we replicate them* — not "a buy happened, buy too". Pipeline:
   `Actor Discovery → Actor Intelligence → Mechanism → Replication → Experiment
   → Shadow → Paper → Live`.

2. **source != strategy != execution.** GMGN is one data source behind a
   `Source` interface. Everything downstream works on `domain.TradeEvent`.
   Adding `source/solana` (direct chain listen) or `source/binance` later
   must not touch the research core.

3. **Quality ≠ Replicability.** Two independent scores:
   - `Quality` — did this actor actually make money? Realized PnL from the
     feed (`amount_usd − buy_cost_usd` on sells), profitable-day share,
     top-1-token concentration, daily-PnL-curve drawdown.
   - `Replicability` — could a follower at our latency capture it? Mean
     markout return of buys at a reference horizon, sample-adjusted.
   A wallet can be rich but uncopyable, or modest but perfectly copyable.
   The project only acts on the latter.

4. **Latency is a first-class measurement.** `SourceAge = ReceivedAt −
   TradeTime` on every event. GMGN REST smart-money feed measured ~140s
   median (2026-08). Any "copy the leader" strategy must survive that —
   the chase table quantifies exactly how much.

5. **DB is the source of truth; memory is a cache.** Dedup: memory TTL +
   `UNIQUE(event_id)`. Cluster state: recomputed from SQLite per event.
   Restart safety falls out of this.

## Pipeline

```text
gmgn_smartmoney ─┐
                 ├─ Collector (limiter+cooldown gate → poll → normalize → dedup → insert)
gmgn_kol ────────┘
                         │  created=true only
                         ▼
            ┌────────────┴──────────────┐
            │ CreateMarkouts (6 horizons)│  UpsertActor   cluster.OnEvent
            └────────────┬──────────────┘
                         ▼
                     SQLite (WAL)
                         ▲
              markout.Engine (30s tick, ≤25 tokens/tick)
              due rows → kline(30s, from/to in ms) per token → FillMarkout
                         │
                         ▼
              analyze: actors rank/inspect, chase, latency, clusters
```

## Rate limiting & the 429 discipline

GMGN (2026-08 实测):

- Leaky bucket documented as rate=20/capacity=20, `RPS = 20 ÷ weight`
  (kline weight 2, smartmoney/kol weight 1).
- Exceeding it triggers **IP-level `RATE_LIMIT_BANNED`** ("IP is temporarily
  banned due to repeated rate limit..."), typically 5 minutes.
- **Every request inside a ban window extends it by 5s** (up to 5 min).
  Retrying is the worst possible response.

FollowEdge's answer: a **shared cooldown gate** (`collector.Limiter`).
Any 429 → `MarkCooldown(reset_at)` → every caller (collectors and the markout
worker) blocks until the gate opens. The gate is a mechanism, not a policy:
the pipeline is structurally incapable of requesting inside a ban window.
Default budget is a conservative 3 weight/s.

Kline quirks: `from`/`to` are Unix **milliseconds** (seconds silently return
an empty list; skill docs are wrong here); without them the API returns only
the latest 50 candles.

## Data flow guarantees

- **Exactly-once pipeline entry.** `InsertEvent` returns `created`; only
  freshly created events reach markout/actor/cluster. Crash+restart replays
  the last poll window — everything old stops at the DB.
- **Out-of-order events are harmless.** Cluster windows are keyed on
  `trade_time`, recomputed fresh each event.
- **Backpressure.** One shared bucket across all requesters.

## Time model

| Instant | Meaning |
|---|---|
| `TradeTime` | on-chain trade time |
| `ReceivedAt` | first seen by our poll |
| `ProcessedAt` | pipeline finished (reserved; == ReceivedAt today) |
| markout `DueAt` | `TradeTime + horizon` |

## Concurrency

- One goroutine per source collector; one markout worker goroutine.
- SQLite: `SetMaxOpenConns(1)` + WAL + `busy_timeout(5000)` — a single
  writer avoids `SQLITE_BUSY` at this scale.

## What is deliberately absent (and why)

- **No signals/orders/positions** — strategy exists only after research
  proves an edge; strategy registry will require `origin_actor/evidence/
  hypothesis/experiment` lineage before any live permission.
- **No enrichment (token/wallet snapshots)** — expensive API calls are only
  justified for candidates; there are no candidates yet.
- **No sub-30s markouts** — GMGN klines bottom out at 30s. Finer horizons
  need a direct chain price feed (v0.2 question; the `SourceAge` data will
  justify whether it's worth building).
