// Behavior reconstruction (v0.2.0): turns position episodes + entry events
// into an actor's BEHAVIOR PROFILE — facts only, no scoring, no archetypes.
//
// Point-in-time discipline carries over from v0.1: entry-context features
// (prior flow, chase) only ever use data knowable at the entry's trade_time.
//
// EVIDENCE POLICY (v0.2.1): every statistic is reported under TWO policies
// side by side, because the strict channel alone is empty by design:
//
//	Strict   — OriginConfirmedZero episodes only. No current data source
//	           can prove a real zero balance (GMGN's is_open_or_close
//	           conflates close/reduce), so this stays n/a until on-chain
//	           balance evidence arrives.
//	Research — VisibleZero + ConfirmedZero episodes (Censored and
//	           data-gap excluded). Inferred, never dressed up as fact.
//
// Censored episodes are counted and their pnl shown separately as
// diagnostics — dataset-coverage information, not mechanism evidence.
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

// TwoStat carries one statistic under the two evidence policies. N=0 on
// both sides means no evidence at all. For ratios (reentry, add-episode,
// cluster>=3, partial-exit, win rate) Value is the ratio and N its
// denominator.
type TwoStat struct {
	Strict   MedianStat // OriginConfirmedZero only
	Research MedianStat // VisibleZero + ConfirmedZero (Censored/DataGap excluded)
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
	// or add). Initial-entry and add features are consumed per evidence
	// policy; Censored means a hidden pre-dataset position may exist.
	OriginQuality storage.OriginQuality
}

// EntryStats are facts about HOW the actor enters positions.
type EntryStats struct {
	InitialCount int // opening buys
	AddCount     int // add buys
	// Opening buys split by origin quality. Only ConfirmedZero feeds the
	// Strict side; VisibleZero is an inference (ledger zero ≠ real zero)
	// and Censored means a hidden position may exist.
	InitialConfirmed int
	InitialVisible   int
	InitialCensored  int

	// ReentryRate: (episodes - distinct tokens) / episodes — how often the
	// actor comes back to a token it already traded (scale-ins are counted
	// separately as adds, not as re-entry).
	ReentryRate TwoStat

	MedianInitialBuy TwoStat // opening buy notional per complete episode
	MedianAddBuy     TwoStat // add notional per episode THAT ADDED (Adds > 0)
	AddEpisodeRate   TwoStat // episodes with adds / episodes
	MedianCapitalIn  TwoStat // total capital in per complete episode

	MedianAge              TwoStat // source age (received - trade), initial entries
	MedianChase            TwoStat // initial entries (entry observations)
	MedianAddChase         TwoStat // adds
	MedianSinceInitialSecs TwoStat // adds: seconds after the opening buy

	SmartPriorP50 TwoStat // prior smart buyers at entry, initial entries
	KOLPriorP50   TwoStat
	Cluster3Plus  TwoStat // share of initial entries with >= 3 prior smart buyers
}

// PositionStats are facts about how the actor manages positions.
// Core statistics are TwoStat (Strict=Confirmed, Research=Visible+Confirmed);
// censored episodes are counted and reported separately.
type PositionStats struct {
	Episodes int // all episodes in the cohort
	Trusted  int // OriginConfirmedZero
	Censored int // everything else (Censored + VisibleZero)

	MedianAdds     TwoStat
	MedianReduces  TwoStat
	MedianHoldSecs TwoStat // closed episodes only
}

// ExitStats are facts about how the actor exits. Core statistics are
// TwoStat; censored pnl and data-gap pnl are MUTUALLY EXCLUSIVE diagnostic
// buckets (a data-gap episode always has OriginCensored — its opening buy
// is unseen — so the three buckets partition realized pnl).
type ExitStats struct {
	PartialExitRatio TwoStat // episodes with PartialExitLegs > 0 / observable exits
	FirstSellP50     TwoStat // seconds from opening buy to first sell leg
	CloseP50         TwoStat // seconds from opening buy to full close (closed only)

	ClosedPnl     TwoStat // Value = realized pnl, N = closed episodes
	ClosedWinRate TwoStat // profitable / closed

	IncompleteRatio float64 // data-gap episodes / all episodes
	IncompletePnl   float64 // realized pnl of data-gap episodes
	CensoredPnl     float64 // realized pnl of Censored, complete-data episodes
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
	var agesStrict, agesResearch, chasesStrict, chasesResearch []float64
	var addChasesStrict, addChasesResearch, sinceStrict, sinceResearch []float64
	var smartStrict, smartResearch, kolStrict, kolResearch []float64
	for _, e := range entries {
		if e.Initial {
			p.Entry.InitialCount++
			switch e.OriginQuality {
			case storage.OriginConfirmedZero:
				p.Entry.InitialConfirmed++
				agesStrict = append(agesStrict, float64(e.ReceivedAt-e.TradeTime))
				agesResearch = append(agesResearch, float64(e.ReceivedAt-e.TradeTime))
				if e.HasChase {
					chasesStrict = append(chasesStrict, e.ChasePct)
					chasesResearch = append(chasesResearch, e.ChasePct)
				}
				if e.PriorValid {
					smartStrict = append(smartStrict, float64(e.SmartPrior))
					smartResearch = append(smartResearch, float64(e.SmartPrior))
					kolStrict = append(kolStrict, float64(e.KOLPrior))
					kolResearch = append(kolResearch, float64(e.KOLPrior))
				}
			case storage.OriginVisibleZero:
				p.Entry.InitialVisible++
				agesResearch = append(agesResearch, float64(e.ReceivedAt-e.TradeTime))
				if e.HasChase {
					chasesResearch = append(chasesResearch, e.ChasePct)
				}
				if e.PriorValid {
					smartResearch = append(smartResearch, float64(e.SmartPrior))
					kolResearch = append(kolResearch, float64(e.KOLPrior))
				}
			default:
				p.Entry.InitialCensored++
			}
		} else {
			p.Entry.AddCount++
			switch e.OriginQuality {
			case storage.OriginConfirmedZero:
				if e.HasChase {
					addChasesStrict = append(addChasesStrict, e.ChasePct)
					addChasesResearch = append(addChasesResearch, e.ChasePct)
				}
				sinceStrict = append(sinceStrict, float64(e.SinceInitialSecs))
				sinceResearch = append(sinceResearch, float64(e.SinceInitialSecs))
			case storage.OriginVisibleZero:
				if e.HasChase {
					addChasesResearch = append(addChasesResearch, e.ChasePct)
				}
				sinceResearch = append(sinceResearch, float64(e.SinceInitialSecs))
			}
		}
	}
	p.Entry.MedianAge = two(median(agesStrict), median(agesResearch))
	p.Entry.MedianChase = two(median(chasesStrict), median(chasesResearch))
	p.Entry.MedianAddChase = two(median(addChasesStrict), median(addChasesResearch))
	p.Entry.MedianSinceInitialSecs = two(median(sinceStrict), median(sinceResearch))
	p.Entry.SmartPriorP50 = two(median(smartStrict), median(smartResearch))
	p.Entry.KOLPriorP50 = two(median(kolStrict), median(kolResearch))
	p.Entry.Cluster3Plus = two(
		ratioStat(smartStrict, 3),
		ratioStat(smartResearch, 3),
	)

	// ---- episodes (sizes, position, exit) ----
	var initStrict, initResearch, capStrict, capResearch []float64
	var addBuyStrict, addBuyResearch []float64
	var addedStrict, addedResearch, episodeN int
	tokens := map[string]bool{}
	tokensStrict := map[string]bool{}
	tokensResearch := map[string]bool{}
	strictN, researchN := 0, 0
	for _, e := range episodes {
		episodeN++
		tokens[e.Token] = true
		strict := e.OriginQuality == storage.OriginConfirmedZero
		research := (e.OriginQuality == storage.OriginConfirmedZero ||
			e.OriginQuality == storage.OriginVisibleZero) && !e.DataGap
		if strict {
			strictN++
			p.Position.Trusted++
			tokensStrict[e.Token] = true
			if e.Adds > 0 {
				addedStrict++
				addBuyStrict = append(addBuyStrict, e.AddBuyUSD)
			}
		}
		if research {
			researchN++
			tokensResearch[e.Token] = true
			if e.Adds > 0 {
				addedResearch++
				addBuyResearch = append(addBuyResearch, e.AddBuyUSD)
			}
		}
		if strict {
			initStrict = append(initStrict, e.InitialBuyUSD)
			capStrict = append(capStrict, e.CapitalIn)
		}
		if research {
			initResearch = append(initResearch, e.InitialBuyUSD)
			capResearch = append(capResearch, e.CapitalIn)
		}
	}
	if episodeN > 0 {
		p.Entry.ReentryRate = two(
			reentryStat(tokensStrict, strictN),
			reentryStat(tokensResearch, researchN),
		)
		p.Entry.AddEpisodeRate = two(
			ratioStatN(addedStrict, strictN),
			ratioStatN(addedResearch, researchN),
		)
	}
	p.Entry.MedianInitialBuy = two(median(initStrict), median(initResearch))
	p.Entry.MedianAddBuy = two(median(addBuyStrict), median(addBuyResearch))
	p.Entry.MedianCapitalIn = two(median(capStrict), median(capResearch))

	p.Position.Episodes = episodeN
	p.Position.Censored = episodeN - strictN
	var addsS, addsR, redS, redR, holdS, holdR []float64
	for _, e := range episodes {
		strict := e.OriginQuality == storage.OriginConfirmedZero
		research := (e.OriginQuality == storage.OriginConfirmedZero ||
			e.OriginQuality == storage.OriginVisibleZero) && !e.DataGap
		if strict {
			addsS = append(addsS, float64(e.Adds))
			redS = append(redS, float64(e.Reduces))
			if e.Status == storage.EpisodeClosed {
				holdS = append(holdS, float64(e.HoldDurationS))
			}
		}
		if research {
			addsR = append(addsR, float64(e.Adds))
			redR = append(redR, float64(e.Reduces))
			if e.Status == storage.EpisodeClosed {
				holdR = append(holdR, float64(e.HoldDurationS))
			}
		}
	}
	p.Position.MedianAdds = two(median(addsS), median(addsR))
	p.Position.MedianReduces = two(median(redS), median(redR))
	p.Position.MedianHoldSecs = two(median(holdS), median(holdR))

	// ---- EXIT ----
	var obsS, obsR, partialS, partialR int
	var firstS, firstR, closeS, closeR []float64
	var closedS, closedR, winsS, winsR int
	var pnlS, pnlR, censoredPnl, gapPnl float64
	var incompleteN int
	for _, e := range episodes {
		if e.DataGap {
			incompleteN++
			gapPnl += e.RealizedPnL // data-gap bucket (always OriginCensored)
		}
		strict := e.OriginQuality == storage.OriginConfirmedZero
		research := (e.OriginQuality == storage.OriginConfirmedZero ||
			e.OriginQuality == storage.OriginVisibleZero) && !e.DataGap
		if !strict && !research {
			if e.Status == storage.EpisodeClosed && !e.DataGap {
				censoredPnl += e.RealizedPnL // censored, complete data
			}
			continue
		}
		if e.SellLegs > 0 && !e.DataGap {
			if strict {
				obsS++
				if e.PartialExitLegs > 0 {
					partialS++
				}
				if e.FirstSellAt > 0 {
					firstS = append(firstS, float64(e.FirstSellAt-e.OpenedAt))
				}
			}
			if research {
				obsR++
				if e.PartialExitLegs > 0 {
					partialR++
				}
				if e.FirstSellAt > 0 {
					firstR = append(firstR, float64(e.FirstSellAt-e.OpenedAt))
				}
			}
		}
		if e.Status == storage.EpisodeClosed {
			if strict {
				closedS++
				pnlS += e.RealizedPnL
				closeS = append(closeS, float64(e.HoldDurationS))
				if e.RealizedPnL > 0 {
					winsS++
				}
			}
			if research {
				closedR++
				pnlR += e.RealizedPnL
				closeR = append(closeR, float64(e.HoldDurationS))
				if e.RealizedPnL > 0 {
					winsR++
				}
			}
		}
	}
	p.Exit.PartialExitRatio = two(
		ratioStatN(partialS, obsS),
		ratioStatN(partialR, obsR),
	)
	p.Exit.FirstSellP50 = two(median(firstS), median(firstR))
	p.Exit.CloseP50 = two(median(closeS), median(closeR))
	p.Exit.ClosedPnl = two(
		MedianStat{Value: pnlS, N: closedS},
		MedianStat{Value: pnlR, N: closedR},
	)
	p.Exit.ClosedWinRate = two(
		ratioStatN(winsS, closedS),
		ratioStatN(winsR, closedR),
	)
	if episodeN > 0 {
		p.Exit.IncompleteRatio = float64(incompleteN) / float64(episodeN)
	}
	p.Exit.IncompletePnl = gapPnl
	p.Exit.CensoredPnl = censoredPnl
	return p
}

func two(s, r MedianStat) TwoStat { return TwoStat{Strict: s, Research: r} }

func reentryStat(tokens map[string]bool, n int) MedianStat {
	if n == 0 {
		return MedianStat{}
	}
	return MedianStat{Value: 1 - float64(len(tokens))/float64(n), N: n}
}

func ratioStat(values []float64, threshold float64) MedianStat {
	n := 0
	for _, v := range values {
		if v >= threshold {
			n++
		}
	}
	return ratioStatN(n, len(values))
}

func ratioStatN(num, den int) MedianStat {
	if den == 0 {
		return MedianStat{}
	}
	return MedianStat{Value: float64(num) / float64(den), N: den}
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
