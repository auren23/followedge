package gmgn

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/auren23/followedge/internal/domain"
)

// NormalizeTrade converts one raw GMGN feed item into a domain.TradeEvent.
// rawJSON is kept verbatim so history can be re-normalized if GMGN changes
// its schema.
func NormalizeTrade(it TradeItem, source string, walletType domain.WalletType, receivedAt time.Time) (domain.TradeEvent, error) {
	if it.TransactionHash == "" || it.Maker == "" || it.BaseAddress == "" {
		return domain.TradeEvent{}, fmt.Errorf("incomplete trade item: tx=%q maker=%q token=%q",
			it.TransactionHash, it.Maker, it.BaseAddress)
	}
	side := domain.Side(it.Side)
	if side != domain.Buy && side != domain.Sell {
		return domain.TradeEvent{}, fmt.Errorf("unknown side %q", it.Side)
	}

	// smartmoney/kol semantics: 0 = opened/added, 1 = closed/reduced.
	action := domain.Open
	if it.IsOpenOrClose == 1 {
		action = domain.Reduce
	}

	raw, err := json.Marshal(it)
	if err != nil {
		return domain.TradeEvent{}, err
	}

	e := domain.TradeEvent{
		ID:             domain.EventID("sol", it.TransactionHash, it.Maker, it.BaseAddress, string(side)),
		Source:         source,
		Chain:          "sol",
		TxHash:         it.TransactionHash,
		Wallet:         it.Maker,
		WalletType:     walletType,
		TokenAddress:   it.BaseAddress,
		TokenSymbol:    it.BaseToken.Symbol,
		Side:           side,
		PositionAction: action,
		AmountUSD:      it.AmountUSD,
		TokenAmount:    it.TokenAmount,
		PriceUSD:       it.PriceUSD,
		BuyCostUSD:     it.BuyCostUSD,
		TradeTime:      time.Unix(it.Timestamp, 0).UTC(),
		ReceivedAt:     receivedAt,
		RawJSON:        string(raw),
	}
	return e, nil
}
