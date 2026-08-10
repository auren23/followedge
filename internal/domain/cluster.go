package domain

import "time"

// ClusterFeatures is a snapshot of how many distinct wallets converged on a
// token within a rolling window. Always distinct wallets — trade count alone
// can be one wallet flipping repeatedly.
type ClusterFeatures struct {
	TokenAddress string
	Window       time.Duration

	SmartBuyWallets  int
	SmartSellWallets int
	KOLBuyWallets    int
	KOLSellWallets   int

	SmartBuyUSD  float64
	SmartSellUSD float64

	FirstEventAt time.Time
	LastEventAt  time.Time
	EventCount   int
}

// NetSmartFlowUSD is buy USD minus sell USD from smart money in the window.
func (c ClusterFeatures) NetSmartFlowUSD() float64 { return c.SmartBuyUSD - c.SmartSellUSD }
