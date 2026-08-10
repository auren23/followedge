// Package domain defines the normalized models shared across FollowEdge.
//
// source != strategy != execution: GMGN is only one possible data source;
// everything downstream of the collector works on these types only.
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

type WalletType string

const (
	WalletSmartMoney WalletType = "smart_money"
	WalletKOL        WalletType = "kol"
)

type Side string

const (
	Buy  Side = "buy"
	Sell Side = "sell"
)

// PositionAction describes what the trade means for the wallet's position.
// The GMGN smartmoney/kol feed only exposes is_open_or_close (0 = opened/added,
// 1 = closed/reduced), so Open/Reduce are the values we can produce today;
// Add/Close are reserved for feeds that distinguish partial events.
type PositionAction string

const (
	Open   PositionAction = "open"
	Add    PositionAction = "add"
	Reduce PositionAction = "reduce"
	Close  PositionAction = "close"
)

// TradeEvent is one normalized smart-money or KOL trade.
type TradeEvent struct {
	ID string // deterministic event_id, see EventID

	Source string // "gmgn_smartmoney" | "gmgn_kol"
	Chain  string // "sol" | "bsc" | ...

	TxHash string

	Wallet     string
	WalletType WalletType

	TokenAddress string
	TokenSymbol  string

	Side           Side
	PositionAction PositionAction

	AmountUSD   float64 // trade notional in USD
	TokenAmount float64
	PriceUSD    float64 // price at trade time
	BuyCostUSD  float64 // original buy cost for sells (0 on buys) — realized PnL input

	TradeTime   time.Time // when the trade happened on-chain
	ReceivedAt  time.Time // when we first saw it
	ProcessedAt time.Time // when the pipeline finished ingesting it

	RawJSON string // the untouched upstream payload, for re-normalization
}

// EventID derives a deterministic ID. The (chain, tx_hash, wallet, token, side)
// tuple is unique per trade in the GMGN feeds.
func EventID(chain, txHash, wallet, token, side string) string {
	h := sha256.Sum256([]byte(chain + "|" + txHash + "|" + wallet + "|" + token + "|" + side))
	return hex.EncodeToString(h[:8])
}

// SourceAge is how stale the trade was when we received it — the GMGN REST
// pipeline's total observation latency.
func (e TradeEvent) SourceAge() time.Duration { return e.ReceivedAt.Sub(e.TradeTime) }
