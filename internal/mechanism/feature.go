// Package mechanism defines the DATA CONTRACT for behavior research.
//
// v0.1.3 ships contracts only — no scoring, no rules, no archetypes. Filling
// these features starts after the mechanism-ready gate (see
// docs/v0.1.3-dataset.md); mining patterns before enough data is data
// snooping.
//
// Every value carries provenance: WHAT was observed, WHEN it was knowable,
// and WHERE it came from. Point-in-time correctness is first-class here —
// a feature observed after decision time must not influence that decision
// (the same discipline that fixed follower entry prices).
package mechanism

import "time"

// Feature is a provenance-tagged value.
type Feature[T any] struct {
	Value T

	// ObservedAt is when the underlying data was actually knowable
	// (NOT when it was fetched — fetching late must not backdate).
	ObservedAt time.Time

	// Source is the origin, e.g. "gmgn_kline", "gmgn_token", "chain".
	Source string

	// Quality labels staleness/confidence: "fresh", "stale", "estimated",
	// "missing".
	Quality string
}

// EntryFeatures describe the market context a trader saw at entry. All
// pointers are nil until the data source exists; nil = not yet available,
// not zero.
type EntryFeatures struct {
	// TokenAge at the actor's entry (creation time needs a token source).
	TokenAge *Feature[time.Duration]

	MarketCapUSD *Feature[float64]
	LiquidityUSD *Feature[float64]

	// PriceChange30s/5m BEFORE the entry (needs kline lookup at entry time).
	PriceChange30s *Feature[float64]
	PriceChange5m  *Feature[float64]

	// Distinct wallet counts that preceded this entry (cluster engine).
	SmartBuysBefore  *Feature[int]
	SmartSellsBefore *Feature[int]
	KOLBuysBefore    *Feature[int]

	// ActorEntryRank: how early in the token's buyer sequence was this
	// actor? Needs a per-token buyer ordering source.
	ActorEntryRank *Feature[int]

	Launchpad *Feature[string]
}

// ExitFeatures describe how the actor exited.
type ExitFeatures struct {
	HoldDuration *Feature[time.Duration]

	// PartialExits / Adds come from position episode reconstruction.
	PartialExits *Feature[int]
	Adds         *Feature[int]

	RealizedReturnPct *Feature[float64] // vs episode capital_in
}

// PositionEpisode is the reconstruction output consumed by ExitFeatures
// (persisted in position_episodes; see storage/episodes.go).
type PositionEpisode struct {
	Wallet        string
	Token         string
	OpenedAt      time.Time
	ClosedAt      *time.Time // nil while open
	Adds          int
	Reduces       int
	CapitalInUSD  float64
	CapitalOutUSD float64
	RealizedPnL   float64
	Status        string // "open" | "closed" | "partial"
}
