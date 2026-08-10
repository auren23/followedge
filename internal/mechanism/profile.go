// Behavior reconstruction (v0.2.0): turns position episodes + entry events
// into an actor's BEHAVIOR PROFILE — facts only, no scoring, no archetypes.
//
// Point-in-time discipline carries over from v0.1: entry-context features
// (prior flow, chase) only ever use data knowable at the entry's trade_time.
// Semantic notes (v0.2.0.1):
//   - "partial exit" = sell legs that LEFT visible quantity (PartialExitLegs),
//     never the data-gap status EpisodePartial;
//   - chase comes from entry observations only (never future-conditioned);
//   - every feature that can be missing carries its sample size (N).
package mechanism

import (
	"sort"

	"github.com/auren23/followedge/internal/storage"
)

// MedianStat is a median with its sample size — 0/0 means "no data", NOT a
// median of zero.
type MedianStat struct {
	Value float64
	N     int
}

// EntryStats are facts about HOW the actor enters positions.
type EntryStats struct {
	Count int // buy events in window

	// ReentryRate: (episodes - distinct tokens) / episodes — how often the
	// actor comes back to a token it already traded (scale-ins are counted
	// separately as adds, not as re-entry).
	ReentryRate float64

	MedianInitialBuy MedianStat // opening buy notional per episode
	MedianAddBuy     MedianStat // add-buy notional per episode
	MedianCapitalIn  MedianStat // total capital in per episode
	MedianAge        MedianStat // median source age (received - trade), seconds

	MedianChase   MedianStat // median entry chase % (entry observations only)
	SmartPriorP50 MedianStat // median prior smart buyers at entry (PIT)
	KOLPriorP50   MedianStat // median prior KOL buyers at entry (PIT)
	Cluster3Plus  float64    // share of entries with >= 3 prior smart buyers
	PriorFlowN    int        // entries with a prior-flow observation
}

// PositionStats are facts about how the actor manages positions.
type PositionStats struct {
	Episodes       int
	MedianAdds     MedianStat
	MedianReduces  MedianStat
	MedianHoldSecs MedianStat // closed episodes only
}

// ExitStats are facts about how the actor exits.
type ExitStats struct {
	// PartialExitRatio: episodes with PartialExitLegs > 0 among episodes
	// with observable exits (at least one sell leg) — REAL partial exits,
	// not data-gap statuses.
	PartialExitRatio float64
	PartialExitN     int
	ObservableN      int // episodes with at least one visible sell leg

	FirstSellP50 MedianStat // seconds from opening buy to first sell leg
	CloseP50     MedianStat // seconds from opening buy to full close (closed only)

	ClosedPnl     float64 // realized pnl of truly closed episodes
	ClosedWinRate float64 // profitable / closed

	IncompleteRatio float64 // data-gap episodes / all episodes
	IncompletePnl   float64 // realized pnl of data-gap episodes (shown separately)
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
	chases []float64, smartPrior, kolPrior []float64) ActorBehaviorProfile {
	p := ActorBehaviorProfile{Wallet: wallet}

	// ---- ENTRY ----
	p.Entry.Count = len(entries)
	tokens := map[string]bool{}
	var ages []float64
	for _, e := range entries {
		tokens[e.Token] = true
		ages = append(ages, float64(e.ReceivedAt-e.TradeTime))
	}
	p.Entry.MedianAge = median(ages)
	if len(episodes) > 0 {
		distinct := 0
		seen := map[string]bool{}
		for _, e := range episodes {
			if !seen[e.Token] {
				seen[e.Token] = true
				distinct++
			}
		}
		p.Entry.ReentryRate = 1 - float64(distinct)/float64(len(episodes))
	}
	var initBuys, addBuys, capIn []float64
	for _, e := range episodes {
		initBuys = append(initBuys, e.InitialBuyUSD)
		addBuys = append(addBuys, e.AddBuyUSD)
		capIn = append(capIn, e.CapitalIn)
	}
	p.Entry.MedianInitialBuy = median(initBuys)
	p.Entry.MedianAddBuy = median(addBuys)
	p.Entry.MedianCapitalIn = median(capIn)
	p.Entry.MedianChase = median(chases)
	p.Entry.SmartPriorP50 = median(smartPrior)
	p.Entry.KOLPriorP50 = median(kolPrior)
	p.Entry.PriorFlowN = len(smartPrior)
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
	var adds, reduces, holds []float64
	for _, e := range episodes {
		adds = append(adds, float64(e.Adds))
		reduces = append(reduces, float64(e.Reduces))
		if e.Status == storage.EpisodeClosed {
			holds = append(holds, float64(e.HoldDurationS))
		}
	}
	p.Position.MedianAdds = median(adds)
	p.Position.MedianReduces = median(reduces)
	p.Position.MedianHoldSecs = median(holds)

	// ---- EXIT ----
	var observable, partialN int
	var firstSells, closes []float64
	var closedN, closedWins int
	var incompleteN int
	for _, e := range episodes {
		if e.DataGap {
			incompleteN++
			p.Exit.IncompletePnl += e.RealizedPnL
		}
		// partial-exit behavior only makes sense for fully visible episodes:
		// a data-gap episode's opening buy is unseen, so its sell timing and
		// exit behavior are not attributable.
		if e.SellLegs > 0 && !e.DataGap {
			observable++
			if e.PartialExitLegs > 0 {
				partialN++
			}
			if e.FirstSellAt > 0 {
				firstSells = append(firstSells, float64(e.FirstSellAt-e.OpenedAt))
			}
		}
		if e.Status == storage.EpisodeClosed {
			closes = append(closes, float64(e.HoldDurationS))
			p.Exit.ClosedPnl += e.RealizedPnL
			closedN++
			if e.RealizedPnL > 0 {
				closedWins++
			}
		}
	}
	p.Exit.FirstSellP50 = median(firstSells)
	p.Exit.CloseP50 = median(closes)
	p.Exit.ObservableN = observable
	if observable > 0 {
		p.Exit.PartialExitRatio = float64(partialN) / float64(observable)
		p.Exit.PartialExitN = partialN
	}
	if closedN > 0 {
		p.Exit.ClosedWinRate = float64(closedWins) / float64(closedN)
	}
	if len(episodes) > 0 {
		p.Exit.IncompleteRatio = float64(incompleteN) / float64(len(episodes))
	}
	return p
}

func median(a []float64) MedianStat {
	if len(a) == 0 {
		return MedianStat{}
	}
	sort.Float64s(a)
	mid := len(a) / 2
	var v float64
	if len(a)%2 == 1 {
		v = a[mid]
	} else {
		v = (a[mid-1] + a[mid]) / 2
	}
	return MedianStat{Value: v, N: len(a)}
}
