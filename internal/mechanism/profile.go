// Behavior reconstruction (v0.2.0): turns position episodes + entry events
// into an actor's BEHAVIOR PROFILE — facts only, no scoring, no archetypes.
//
// Point-in-time discipline carries over from v0.1: entry-context features
// (cluster state, chase) only ever use data knowable at the entry's
// trade_time. The profile is descriptive (what does this actor DO), not
// prescriptive (what should we copy) — archetypes and hypotheses come later.
package mechanism

import (
	"sort"

	"github.com/auren23/followedge/internal/storage"
)

// EntryStats are facts about HOW the actor enters positions.
type EntryStats struct {
	Count         int     // buy events in window
	TokenReuse    float64 // 0-1: (entries - distinct tokens) / entries; 0 = never re-buys
	MedianSize    float64 // median first-buy USD notional per episode
	MedianAge     float64 // median source age (received - trade), seconds
	MedianChase   float64 // median follower entry chase % (from markouts)
	SmartPriorP50 float64 // median smart_buy_wallets visible at entry (PIT)
	KOLPriorP50   float64 // median kol_buy_wallets visible at entry (PIT)
	Cluster3Plus  float64 // 0-1 share of entries with >= 3 prior smart buyers
}

// PositionStats are facts about how the actor manages positions.
type PositionStats struct {
	Episodes         int
	MedianAdds       float64
	MedianReduces    float64
	MedianCapitalIn  float64
	MedianCapitalOut float64
	MedianHoldSecs   float64 // closed episodes only
}

// ExitStats are facts about how the actor exits.
type ExitStats struct {
	PartialRatio        float64 // partial-status episodes / all episodes
	FullRatio           float64 // closed episodes / all episodes
	MedianFirstSellSecs float64 // seconds from opening buy to first sell leg
	MedianCloseSecs     float64 // seconds from opening buy to full close
	TotalPnl            float64
	ProfitableRatio     float64 // closed episodes with pnl > 0
}

// ActorBehaviorProfile is the complete fact card of one actor's behavior.
type ActorBehaviorProfile struct {
	Wallet   string
	Entry    EntryStats
	Position PositionStats
	Exit     ExitStats
}

// BuildProfile computes the behavior profile from raw facts. All inputs are
// pre-filtered to the same window by the caller.
func BuildProfile(wallet string, episodes []storage.Episode, entries []storage.EntryRow,
	firstSellSecs []int64, chasePcts []float64, smartPrior, kolPrior []float64) ActorBehaviorProfile {
	p := ActorBehaviorProfile{Wallet: wallet}

	// ---- ENTRY ----
	p.Entry.Count = len(entries)
	tokens := map[string]bool{}
	var sizes []float64
	var ages []float64
	for _, e := range entries {
		tokens[e.Token] = true
		sizes = append(sizes, e.AmountUSD)
		ages = append(ages, float64(e.ReceivedAt-e.TradeTime))
	}
	if p.Entry.Count > 0 {
		p.Entry.TokenReuse = 1 - float64(len(tokens))/float64(p.Entry.Count)
	}
	p.Entry.MedianSize = medianF(sizes)
	p.Entry.MedianAge = medianF(ages)
	p.Entry.MedianChase = medianF(chasePcts)
	p.Entry.SmartPriorP50 = medianF(smartPrior)
	p.Entry.KOLPriorP50 = medianF(kolPrior)
	if len(smartPrior) > 0 {
		n3 := 0
		for _, n := range smartPrior {
			if n >= 3 {
				n3++
			}
		}
		p.Entry.Cluster3Plus = float64(n3) / float64(len(smartPrior))
	}

	// ---- POSITION ----
	p.Position.Episodes = len(episodes)
	var adds, reduces, capIn, capOut, holds []float64
	for _, e := range episodes {
		adds = append(adds, float64(e.Adds))
		reduces = append(reduces, float64(e.Reduces))
		capIn = append(capIn, e.CapitalIn)
		capOut = append(capOut, e.CapitalOut)
		if e.ClosedAt > 0 {
			holds = append(holds, float64(e.HoldDurationS))
		}
	}
	p.Position.MedianAdds = medianF(adds)
	p.Position.MedianReduces = medianF(reduces)
	p.Position.MedianCapitalIn = medianF(capIn)
	p.Position.MedianCapitalOut = medianF(capOut)
	p.Position.MedianHoldSecs = medianF(holds)

	// ---- EXIT ----
	if len(episodes) > 0 {
		closed, partial := 0, 0
		for _, e := range episodes {
			switch e.Status {
			case storage.EpisodeClosed:
				closed++
			case storage.EpisodePartial:
				partial++
			}
		}
		p.Exit.PartialRatio = float64(partial) / float64(len(episodes))
		p.Exit.FullRatio = float64(closed) / float64(len(episodes))
	}
	p.Exit.MedianFirstSellSecs = medianF(toF64(firstSellSecs))
	var closeSecs []float64
	var wins int
	var winsN int
	for _, e := range episodes {
		if e.ClosedAt > 0 {
			closeSecs = append(closeSecs, float64(e.HoldDurationS))
			p.Exit.TotalPnl += e.RealizedPnL
			winsN++
			if e.RealizedPnL > 0 {
				wins++
			}
		}
	}
	p.Exit.MedianCloseSecs = medianF(closeSecs)
	if winsN > 0 {
		p.Exit.ProfitableRatio = float64(wins) / float64(winsN)
	}
	return p
}

func medianF(a []float64) float64 {
	if len(a) == 0 {
		return 0
	}
	sort.Float64s(a)
	mid := len(a) / 2
	if len(a)%2 == 1 {
		return a[mid]
	}
	return (a[mid-1] + a[mid]) / 2
}

func toF64(a []int64) []float64 {
	out := make([]float64, len(a))
	for i, v := range a {
		out[i] = float64(v)
	}
	return out
}
