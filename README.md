# FollowEdge

**Profit Actor Discovery & Strategy Replication Engine.**

版本：`v0.1.3.2-dataset-integrity`。

核心问题不是"哪些信号可以买"，而是：

> 谁真实赚到了钱 → 他靠什么赚钱 → 这个 edge 是否可复制 → 我们能否在自己的延迟/资金/成本下复现？

```text
GMGN
 │
 ▼
Collector (poll + dedup + restart-safe + IP-ban-safe)
 │
 ▼
Event Database (SQLite, WAL)
 │
 ├──────────────► Actor Discovery  (谁在交易？)
 │                    └── realized PnL / consistency / concentration / drawdown
 │
 ├──────────────► Actor Intelligence (Quality score)
 │
 ├──────────────► Markout Engine  (每笔交易的 forward return @ 30s..1h)
 │                    └── Alpha Decay / chase / Replicability score
 │
 └──────────────► Cluster Engine (rolling windows, 供未来 mechanism mining)
```

**v0.1 定位：GMGN Profit Actor Research Engine。** 只做一件事：找到
`profitable + consistent + copyable` 的 actor，用 `followedge actors rank`
输出。不产生任何交易。

## 两个分数，必须分开看

| | 回答的问题 | 依据 |
|---|---|---|
| **Quality** (0-100) | 他赚到钱了吗？ | realized PnL（卖单 `amount_usd − buy_cost_usd`）、盈利日占比、top-1 代币集中度、日 PnL 曲线回撤 |
| **Replicability** (0-100) | 我们晚 N 秒进场还能赚吗？ | 买单 markout 平均收益（参考 horizon）+ 样本量调整 |

**Quality 高 ≠ 可以跟。** 彩票选手（一个代币赚 90% 的利润）会被
concentration 降权；EV 已衰减的 wallet 会被 replicability 归零。

## Quickstart

```bash
# 1. API key: $GMGN_API_KEY，或复用 ~/.config/gmgn/.env（gmgn-cli 的配置）
export GMGN_API_KEY=...

# 2. Build & collect（Ctrl-C 停止；--once 只跑一轮）
make build
bin/followedge collect --config configs/observe.yaml

# 3. 研究
bin/followedge status
bin/followedge actors rank --horizon 60s          # 核心输出：找可复制 actor
bin/followedge actors inspect <wallet>            # 单个 actor：PnL 事实 + alpha decay
bin/followedge analyze latency                    # 数据源延迟分布（P50/P90/P95/P99）
bin/followedge analyze chase --horizon 30s        # EV cliff 表
bin/followedge analyze clusters --window 60s      # 钱包汇聚分布
```

## 命令

| 命令 | 用途 |
|---|---|
| `collect` | 采集 smart money + KOL，去重入库，聚类，采样 markout |
| `collect --once` | 每源单轮轮询（冒烟测试） |
| `status` | 行数统计 |
| `actors rank` | Actor 排行榜：Quality 与 Replicability 双轴 |
| `actors inspect <wallet>` | 单个 actor 研究卡：PnL 事实 + 证据等级 + 各 horizon EV 衰减 |
| `analyze latency` | 源延迟分布（trade_time → received_at） |
| `analyze latency-ev` | **source-age × follower EV**（DUE/FILLED/COVER + 保守 EV；`--by-chase` 二维矩阵）|
| `analyze coverage` | markout 状态普查（filled/no_candle/token_inactive/...）—— selection bias 防线 |
| `analyze episodes` | 重建 position episodes（adds/reduces/pnl/hold）+ 统计 |
| `analyze chase` | 追价桶 vs 前向收益 —— EV cliff 表 |
| `analyze clusters` | 窗口内 distinct wallet 汇聚分布 |
| `version` | 版本 |

## 真实数据长什么样（2026-08-10 实测，Solana）

```text
SOURCE AGE (seconds: TradeTime -> ReceivedAt)
wallet_type        N     mean      P50      P90      P95      P99
kol              107    415.1    501.0    745.8    829.6    899.3
smart_money      102    137.8    141.0    237.7    242.9    251.0

CHASE @ 30s — follower EV vs entry chase (entry = price at ReceivedAt)
chase           N       WR        avg     median
<0%            95    41.1%     -5.26%     -2.09%
0-2%            3    66.7%    +18.22%     +3.31%
2-5%            6    50.0%    +16.66%     +5.44%
5-10%           5    40.0%     +0.86%     -0.03%
10-20%          5    40.0%    -13.22%     -3.39%
20%+            8    37.5%     -2.28%    -18.35%
```

**测量口径（v0.1.1 起，见 docs/v0.1.1-measurement.md）**：`chase =
入场价/leader价 − 1`（入场价 = 收到消息时刻的 kline 价格），`follower
EV = horizon 后价格/入场价 − 1`。两个变量分离：分桶按 chase，统计按
follower EV —— 回答"追多高就晚了"。GMGN REST 中位延迟 ~140s，任何
edge 都必须扛住它。样本量远不够下结论，继续积累 5,000+ events。

## 与 GMGN API 相处的实测经验（2026-08 实测）

1. **限流是 IP 级 ban**：`RATE_LIMIT_BANNED`（`"IP is temporarily banned
   due to repeated rate limit..."`）。文档口径 rate=20/capacity=20（漏桶，
   RPS = 20 ÷ weight；kline weight 2），但**冷却期内每个请求 +5s 延长**
   封禁，最多 5 分钟。不要在 429 后重试。
2. **全管线共享冷却闸**：任何请求 429 → 闸门关闭到 `reset_at`（未知则至少
   30s），collector 和 markout worker 全部冻结。默认限速 3 weight/s（保守）。
3. **kline 的 `from`/`to` 单位是毫秒**（skill 文档写秒是错的，传秒返回空
   列表）。不传 from/to 时只返回最近 50 根蜡烛（30s 分辨率 ≈ 25 分钟）。
4. IPv6 请求会被拒，client 强制 tcp4。

## 设计决策（v0.1）

| 灰区 | 决策 |
|---|---|
| realized PnL | GMGN 卖单自带 `buy_cost_usd`（原买入成本）→ `amount_usd − buy_cost_usd`，零额外 API 调用 |
| markout 价格源 | GMGN 30s kline（公开 API 最细粒度），horizon ≥ 30s；更细需要链上价格源（v0.2 研究问题） |
| dedup | 内存 TTL + DB `UNIQUE(event_id)` 双闸；只有 `created=true` 事件进管线 → 重启安全 |
| cluster 状态 | 每次事件从 DB 重算并 append 快照，无内存态漂移 |
| Quality/Replicability 公式 | v0.1 显式启发式（无 ML）：profit 线性到 $10k、盈利日占比、样本量、top1 集中度、日曲线回撤；EV 线性到 +10%、20 fills 满样本 |
| event_id | `sha256(chain\|tx_hash\|wallet\|token\|side)[:16]` |

## Roadmap

| 版本 | 内容 |
|---|---|
| `v0.1.0-observe` | Actor 采集/排名（Quality+Replicability）、markout、chase、cluster、延迟分析 |
| `v0.1.1-measurement` | 测量口径修复：leader/follower markout 分离、chase 重定义、follower EV、buy_cost_usd 自洽性 |
| `v0.1.2-entry-pit` | entry point-in-time（无 look-ahead）、source-age×EV 分析、ActorEvidence E0-E4、survival 指标 |
| `v0.1.3-dataset` | markout status 分类 + coverage 表、coverage-aware EV、conservative EV、position episodes、mechanism 数据契约 |
| `v0.1.3.1-measurement-integrity` | 测量完整性：census 只含 DUE 行、`--side` 真过滤、engine 全分支 status 分类、market outcome vs measurement failure 两维度、migration 007 backfill、kline lookback 2 resolutions、migration 版本追踪修复 |
| `v0.1.3.2-dataset-integrity`（本版） | legacy schema inference（真实版本推断而非 pin maxVer）、pending-due 归入 unresolved 不进 cons-EV、price_parse_error 真正分类、stale_outcome + outcome_observed_at 固定 horizon 口径 |
| `v0.2.0-mechanism` | Mechanism Analyzer（他靠什么赚钱）、Hypothesis 注册、archetype 聚类 |
| `v0.3.0-experiment` | Replay/Experiment Engine（train/val/test）、Strategy 血缘注册 |
| `v0.4.0-shadow` | 实时 Shadow Copy + Strategy Clone |
| `v0.5.0-paper` | PaperBroker、PositionManager、退出实验 |
| `v0.6.0-live` | RiskEngine、kill switch、Tiny Live（`origin_*` 齐全才允许） |

**纪律：** 任何策略必须带 `origin_actor / origin_evidence / origin_hypothesis /
origin_experiment / historical / out_of_sample / shadow / paper` 结果，
缺任何一环禁止 live。

## 项目结构

```text
cmd/followedge/       CLI
internal/domain/      归一化模型（source 无关）
internal/source/gmgn/ GMGN OpenAPI adapter（全仓库唯一认识 GMGN 的地方）
internal/collector/   轮询、限流（token bucket + 冷却闸）、dedup
internal/cluster/     rolling-window 汇聚（未来 mechanism 特征输入）
internal/markout/     forward-return 采样（alpha decay 原料）
internal/analyze/     actors rank/inspect、latency、chase、clusters
internal/storage/     SQLite (WAL) + 版本化迁移
configs/              YAML 配置
docs/                 架构与数据模型
```

## Testing

```bash
make test
```

覆盖：normalize（真实抓包 fixture）、dedup TTL、限流器（含冷却闸）、
distinct-wallet 聚类、markout 采样、429 冻结管线、actor 聚合与评分。

## Disclaimer

研究工具。不构成投资建议。v0.1 不下任何订单 —— 在 markout 数据证明 edge
扛得住延迟、滑点和手续费之前，也不应该下。
