package gmgn

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/auren23/followedge/internal/domain"
)

// Real payload captured from /v1/user/smartmoney on 2026-04-18.
const fixture = `{"transaction_hash":"8YQRwZR5UXpcoxXQDEimoBwmQgmtBh3nH3zsDZo59rNcEHYEQUAKNKf4F1KKRtBryhGoee3yJu9bSCRWMHu9CRH","maker":"GAAcxw3qcRmQTzm34UGQLz6q5V33Wk7vyugMPWXZKAbn","base_amount":1343817.453573,"quote_amount":1.234442131,"buy_cost_usd":119.11234618215,"token_amount":1343817.453573,"amount_usd":95.06438850831,"price":9.186084968e-07,"price_usd":0.000070742040338568,"timestamp":1786348916,"side":"sell","is_open_or_close":1,"base_address":"5kr3KPg6Nhx2cBvunCuEgnUnJzRRz7C56aKNaFMmpump","balance":0,"base_token":{"symbol":"Shitcoin","logo":"https://gmgn.ai/external-res/x.webp","total_supply":"984313405","launchpad":"pump"},"maker_info":{"avatar":"","name":"","tags":["smart_degen"],"twitter_username":"","twitter_name":""}}`

func TestNormalizeTrade(t *testing.T) {
	var it TradeItem
	if err := json.Unmarshal([]byte(fixture), &it); err != nil {
		t.Fatal(err)
	}
	received := time.Unix(1786348918, 0).UTC()
	e, err := NormalizeTrade(it, "gmgn_smartmoney", domain.WalletSmartMoney, received)
	if err != nil {
		t.Fatal(err)
	}
	if e.Side != domain.Sell || e.PositionAction != domain.Reduce {
		t.Errorf("side/action = %s/%s, want sell/reduce (is_open_or_close=1)", e.Side, e.PositionAction)
	}
	if e.Wallet != "GAAcxw3qcRmQTzm34UGQLz6q5V33Wk7vyugMPWXZKAbn" || e.TokenSymbol != "Shitcoin" {
		t.Errorf("wallet/symbol mismatch: %s/%s", e.Wallet, e.TokenSymbol)
	}
	if e.AmountUSD != 95.06438850831 || e.PriceUSD != 0.000070742040338568 {
		t.Errorf("amounts mismatch: %v %v", e.AmountUSD, e.PriceUSD)
	}
	if e.SourceAge() != 2*time.Second {
		t.Errorf("source age = %v, want 2s", e.SourceAge())
	}
	if e.RawJSON == "" {
		t.Error("raw json not preserved")
	}
	if e.ID == "" || e.ID != domain.EventID("sol", e.TxHash, e.Wallet, e.TokenAddress, string(e.Side)) {
		t.Errorf("event id mismatch: %q", e.ID)
	}
}

func TestNormalizeOpenIsOpenOrCloseZero(t *testing.T) {
	var it TradeItem
	if err := json.Unmarshal([]byte(fixture), &it); err != nil {
		t.Fatal(err)
	}
	it.IsOpenOrClose = 0
	e, err := NormalizeTrade(it, "gmgn_smartmoney", domain.WalletSmartMoney, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if e.PositionAction != domain.Open {
		t.Errorf("action = %s, want open (is_open_or_close=0)", e.PositionAction)
	}
}

func TestNormalizeRejectsIncomplete(t *testing.T) {
	_, err := NormalizeTrade(TradeItem{}, "gmgn_smartmoney", domain.WalletSmartMoney, time.Now().UTC())
	if err == nil {
		t.Fatal("expected error for empty item")
	}
}
