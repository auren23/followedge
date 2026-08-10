// Behavior reconstruction (v0.2.0): turns position episodes + entry events
// into an actor's BEHAVIOR PROFILE — facts only, no scoring, no archetypes.
//
// Point-in-time discipline carries over from v0.1: entry-context features
// (prior flow, chase) only ever use data knowable at the entry's trade_time.
// Semantic notes (v0.2.0.2):
//   - "partial exit" = sell legs that LEFT visible quantity (PartialExitLegs),
//     never the data-gap status EpisodePartial;
//   - chase comes from entry observations only (never future-conditioned)
//     and is horizon-independent;
//   - INITIAL entry context and ADD context are separate questions — why he
//     opens vs why he adds must not be conflated;
//   - data-gap episodes never enter initial-buy / capital medians;
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

// EntryFact is one buy event's entry-time context (pre-classified by the
// caller from the full event stream).
type EntryFact struct {
	Initial          bool // opening buy of an episode, not an add
	TradeTime        int64
	ReceivedAt       int64
	ChasePct         float64
	HasChase         bool
	SmartPrior       int
	KOLPrior         int
	PriorValid       bool  // prior-flow window lies inside the dataset (not truncated)
	SinceInitialSecs int64 // adds only: time since the episode's opening buy
	// OriginQuality rates the evidence behind this buy's episode (initial
	// or add). Initial-entry and add features below are only consumed from
	// OriginConfirmedZero episodes — the only level that proves the wallet
	// actually had no position before. VisibleZero is inferred, research
	// only; Censored means a hidden pre-dataset position may exist.
	OriginQuality storage.OriginQuality
}

// EntryStats are facts about HOW the actor enters positions.
type EntryStats struct {
	InitialCount int // opening buys
	AddCount     int // add buys
	// Opening buys split by origin quality. Only ConfirmedZero feeds the
	// initial-entry features below; VisibleZero is an inference (ledger
	// zero ≠ real zero) and Censored means a hidden position may exist.
	InitialConfirmed int
	InitialVisible   int
	InitialCensored  int

	// ReentryRate: (episodes - distinct tokens) / episodes — how often the
	// actor comes back to a token it already traded (scale-ins are counted
	// separately as adds, not as re-entry).
	ReentryRate float64

	MedianInitialBuy MedianStat // opening buy notional per COMPLETE, confirmed episode
	MedianAddBuy     MedianStat // add notional per episode THAT ADDED (Adds > 0)
	AddEpisodeRate   float64    // episodes with adds / episodes
	MedianCapitalIn  MedianStat // total capital in per COMPLETE, confirmed episode

	MedianAge              MedianStat // initial entries only: source age (received - trade)
	MedianChase            MedianStat // initial entries only (entry observations)
	MedianAddChase         MedianStat // adds only
	MedianSinceInitialSecs MedianStat // adds to CONFIRMED episodes only: seconds after opening

	SmartPriorP50 MedianStat // confirmed initial entries with a valid prior-flow window
	KOLPriorP50   MedianStat
	Cluster3Plus  float64 // share of valid-prior confirmed initial entries with >= 3 prior smart buyers
	PriorFlowN    int     // confirmed initial entries with valid prior flow
}

// PositionStats are facts about how the actor manages positions.
// Core statistics consume CONFIRMED episodes only; censored episodes
// (left-censored or inferred-zero) are counted and shown separately.
type PositionStats struct {
	Episodes       int // all episodes in the cohort
	Trusted        int // OriginConfirmedZero
	Censored       int // everything else (Censored + VisibleZero)
	MedianAdds     MedianStat
	MedianReduces  MedianStat
	MedianHoldSecs MedianStat // closed, confirmed episodes only
}

// ExitStats are facts about how the actor exits.
type ExitStats struct {
	// PartialExitRatio: episodes with PartialExitLegs > 0 among fully
	// visible episodes with observable exits — REAL partial exits, not
	// data-gap statuses.
	PartialExitRatio float64
	PartialExitN     int
	ObservableN      int // fully visible episodes with at least one sell leg

	FirstSellP50 MedianStat // seconds from opening buy to first sell leg
	CloseP50     MedianStat // seconds from opening buy to full close (closed only)

	ClosedPnl     float64 // realized pnl of truly closed episodes
	ClosedWinRate float64 // profitable / closed

	IncompleteRatio float64 // data-gap episodes / all episodes
	IncompletePnl   float64 // realized pnl of data-gap episodes (shown separately)

	CensoredPnl float64 // realized pnl of censored episodes (shown separately)
}

// ActorBehaviorProfile is the complete fact card of one actor's behavior.
type ActorBehaviorProfile struct {
	Wallet   string
	Entry    EntryStats
	Position PositionStats
	Exit     ExitStats
}

// BuildProfile computes the behavior profile from raw facts. Episodes must
// already be filtered to the analysis window; entries carry the initial/add
// classification and PIT prior flow.
func BuildProfile(wallet string, episodes []storage.Episode, entries []EntryFact) ActorBehaviorProfile {
	p := ActorBehaviorProfile{Wallet: wallet}

	// ---- ENTRY ----
	var ages, chases, addChases, sinceInitial []float64
	var smartPrior, kolPrior []float64
	for _, e := range entries {
		if e.Initial {
			p.Entry.InitialCount++
			switch e.OriginQuality {
			case storage.OriginConfirmedZero:
				p.Entry.InitialConfirmed++
				ages = append(ages, float64(e.ReceivedAt-e.TradeTime))
				if e.HasChase {
					chases = append(chases, e.ChasePct)
				}
				if e.PriorValid {
					smartPrior = append(smartPrior, float64(e.SmartPrior))
					kolPrior = append(kolPrior, float64(e.KOLPrior))
				}
			case storage.OriginVisibleZero:
				p.Entry.InitialVisible++
			default:
				p.Entry.InitialCensored++
			}
		} else {
			p.Entry.AddCount++
			// adds of a left-censored episode are untrustworthy too — the
			// "opening" they add to may not be the real opening
			if e.OriginQuality == storage.OriginConfirmedZero {
				if e.HasChase {
					addChases = append(addChases, e.ChasePct)
				}
				sinceInitial = append(sinceInitial, float64(e.SinceInitialSecs))
			}
		}
	}
	p.Entry.MedianAge = median(ages)
	p.Entry.MedianChase = median(chases)
	p.Entry.MedianAddChase = median(addChases)
	p.Entry.MedianSinceInitialSecs = median(sinceInitial)
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

	tokens := map[string]bool{}
	var initBuys, addBuys, capIn []float64
	var addedEpisodes, episodeN int
	for _, e := range episodes {
		tokens[e.Token] = true
		episodeN++
		// data-gap episodes have no visible opening buy, and anything below
		// OriginConfirmedZero may really be an add to a hidden position:
		// neither may enter the initial-size medians as a true initial entry.
		if !e.DataGap && e.OriginQuality == storage.OriginConfirmedZero {
			initBuys = append(initBuys, e.InitialBuyUSD)
			capIn = append(capIn, e.CapitalIn)
		}
		if e.Adds > 0 {
			addedEpisodes++
			addBuys = append(addBuys, e.AddBuyUSD)
		}
	}
	if episodeN > 0 {
		p.Entry.AddEpisodeRate = float64(addedEpisodes) / float64(episodeN)
		p.Entry.ReentryRate = 1 - float64(len(tokens))/float64(episodeN)
	}
	p.Entry.MedianInitialBuy = median(initBuys)
	p.Entry.MedianAddBuy = median(addBuys)
	p.Entry.MedianCapitalIn = median(capIn)

	// ---- POSITION ----
	p.Position.Episodes = episodeN
	var adds, reduces, holds []float64
	for _, e := range episodes {
		trusted := e.OriginQuality == storage.OriginConfirmedZero
		if trusted {
			p.Position.Trusted++
			adds = append(adds, float64(e.Adds))
			reduces = append(reduces, float64(e.Reduces))
			if e.Status == storage.EpisodeClosed {
				holds = append(holds, float64(e.HoldDurationS))
			}
		} else {
			p.Position.Censored++
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
		trusted := e.OriginQuality == storage.OriginConfirmedZero
		if e.DataGap {
			incompleteN++
			p.Exit.IncompletePnl += e.RealizedPnL
		}
		if !trusted {
			// closed AND data-gap-partial episodes carry realized pnl
			if e.Status == storage.EpisodeClosed || e.Status == storage.EpisodePartial {
				p.Exit.CensoredPnl += e.RealizedPnL
			}
			continue // core exit statistics consume confirmed episodes only
		}
		// partial-exit behavior only makes sense for fully visible episodes:
		// a data-gap episode's opening buy is unseen, so its exit behavior
		// is not attributable.
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
	if episodeN > 0 {
		p.Exit.IncompleteRatio = float64(incompleteN) / float64(episodeN)
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
