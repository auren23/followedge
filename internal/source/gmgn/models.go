package gmgn

import (
	"context"
	"net/url"
	"strconv"
	"time"
)

// TradeItem is the raw shape of one record in /v1/user/smartmoney and
// /v1/user/kol (identical schemas). Only fields FollowEdge needs are declared;
// the rest survives in the raw JSON captured by the collector.
type TradeItem struct {
	TransactionHash string `json:"transaction_hash"`
	Maker           string `json:"maker"`
	Side            string `json:"side"`
	BaseAddress     string `json:"base_address"`

	BaseAmount  float64 `json:"base_amount"`
	TokenAmount float64 `json:"token_amount"`
	AmountUSD   float64 `json:"amount_usd"`
	BuyCostUSD  float64 `json:"buy_cost_usd"`
	PriceUSD    float64 `json:"price_usd"`

	// 0 = position opened/added, 1 = position closed/reduced (smartmoney/kol
	// semantics — the opposite of follow_wallet).
	IsOpenOrClose int `json:"is_open_or_close"`

	Timestamp int64 `json:"timestamp"` // unix seconds

	BaseToken struct {
		Symbol    string `json:"symbol"`
		Launchpad string `json:"launchpad"`
	} `json:"base_token"`

	MakerInfo struct {
		Tags []string `json:"tags"`
	} `json:"maker_info"`
}

type tradeFeed struct {
	List []TradeItem `json:"list"`
}

// SmartMoney polls recent smart-money trades. limit: 1-200.
func (c *Client) SmartMoney(ctx context.Context, chain string, limit int) ([]TradeItem, error) {
	q := url.Values{"chain": {chain}, "limit": {strconv.Itoa(limit)}}
	var f tradeFeed
	if err := c.get(ctx, "/v1/user/smartmoney", q, &f); err != nil {
		return nil, err
	}
	return f.List, nil
}

// KOL polls recent KOL trades. limit: 1-200.
func (c *Client) KOL(ctx context.Context, chain string, limit int) ([]TradeItem, error) {
	q := url.Values{"chain": {chain}, "limit": {strconv.Itoa(limit)}}
	var f tradeFeed
	if err := c.get(ctx, "/v1/user/kol", q, &f); err != nil {
		return nil, err
	}
	return f.List, nil
}

// Kline returns 30s-resolution candles. Each candle: Time (unix ms, open
// time), Close (USD). The kline feed is the price source for markouts.
type Candle struct {
	Time  int64  `json:"time"`
	Close string `json:"close"`
}

// Kline fetches candles for [from, to]. NOTE: from/to are Unix MILLISECONDS
// — seconds silently return an empty list (verified empirically 2026-08;
// the skill docs say "seconds", which is wrong).
func (c *Client) Kline(ctx context.Context, chain, address, resolution string, from, to time.Time) ([]Candle, error) {
	q := url.Values{
		"chain":      {chain},
		"address":    {address},
		"resolution": {resolution},
		"from":       {strconv.FormatInt(from.UnixMilli(), 10)},
		"to":         {strconv.FormatInt(to.UnixMilli(), 10)},
	}
	var f struct {
		List []Candle `json:"list"`
	}
	if err := c.get(ctx, "/v1/market/token_kline", q, &f); err != nil {
		return nil, err
	}
	return f.List, nil
}
