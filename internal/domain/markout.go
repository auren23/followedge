package domain

import "time"

// Markout is the forward return of a trade at one horizon: what a copy-trader
// entering exactly `horizon` after the leader would have paid vs. the leader.
// It is the raw material for every EV/copyability/chase conclusion.
type Markout struct {
	EventID       string
	Horizon       time.Duration
	BasePrice     float64 // leader's price (price_usd of the event)
	ObservedPrice float64 // first available price at/after TradeTime+Horizon
	ReturnPct     float64 // (Observed/Base - 1) * 100
}
