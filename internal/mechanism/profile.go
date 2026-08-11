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
//
// ENTRY↔EPISODE LINEAGE (v0.2.1.1): entries carry their episode's FINAL
// evidence (OriginQuality + DataGap, assigned at episode finalize by the
// unified reconstruction walker). An entry can never be research-eligible
// while its episode is not — a DataGap episode excludes its opening buy
// and its adds together, never one without the other. Evidence counts
// everywhere use the single EvidenceCounts provenance.
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

// EvidenceCounts is the unified provenance tally (v0.2.1.1): every episode
// or buy lands in EXACTLY ONE of four mutually exclusive buckets, so
// StrictN / ResearchN mean the same thing everywhere — matrix, CLI,
// hypothesis — instead of being re-derived ad hoc per call site.
type EvidenceCounts struct {
	Confirmed int // OriginConfirmedZero, complete trajectory
	Visible   int // OriginVisibleZero, complete trajectory — inferred, research only
	Censored  int // OriginCensored, complete trajectory (hidden position may exist)
	DataGap   int // trajectory gap (oversold / unseen opening) — never research
}

// StrictN is the independently-proven channel. Empty by design: no current
// source can prove a real zero balance, so Confirmed stays 0 on the
// production path.
func (c EvidenceCounts) StrictN() int { return c.Confirmed }

// ResearchN is the inferred channel: VisibleZero + ConfirmedZero, DataGap
// excluded. Research use only, never dressed up as fact.
func (c EvidenceCounts) ResearchN() int { return c.Confirmed + c.Visible }

// add buckets one observation by origin quality and trajectory gap (a gap
// always wins — the trajectory is broken regardless of origin).
func (c *EvidenceCounts) add(q storage.OriginQuality, gap bool) {
	if gap {
		c.DataGap++
		return
	}
	switch q {
	case storage.OriginConfirmedZero:
		c.Confirmed++
	case storage.OriginVisibleZero:
		c.Visible++
	default:
		c.Censored++
	}
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
	// OriginQuality and DataGap are the entry's EPISODE's final evidence
	// (v0.2.1.1 lineage): assigned at episode finalize, so an entry can
	// never claim research eligibility its episode later lost.
	OriginQuality storage.OriginQuality
	DataGap       bool
}

// EntryStats are facts about HOW the actor enters positions.
type EntryStats struct {
	InitialCount int // opening buys
	AddCount     int // add buys
	// Opening buys / add buys split by the four mutually exclusive evidence
	// buckets. Only Confirmed feeds the Strict side; Visible is an inference
	// (ledger zero ≠ real zero); Censored means a hidden position may exist;
	// DataGap means the episode's trajectory broke (never research).
	Initial EvidenceCounts
	Add     EvidenceCounts

	// ReentryRate: episodes that came back to a token it already had an
	// episode on (IsReentry, fixed over full history) / episodes. The
	// evidence policy decides which episodes ENTER the denominator — it
	// never changes the feature itself, so a re-entry from a censored
	// first episode still counts as a re-entry.
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
// Evidence carries the full episode provenance split (confirmed / inferred /
// censored / gap — mutually exclusive, so StrictN() and ResearchN() are
// well-defined).
type PositionStats struct {
	Episodes int
	Evidence EvidenceCounts

	MedianAdds     TwoStat
	MedianReduces  TwoStat
	MedianHoldSecs TwoStat // closed episodes only
}

// ExitStats are facts about how the actor exits. Core statistics are
// TwoStat; censored pnl and data-gap pnl are MUTUALLY EXCLUSIVE diagnostic
// buckets of CLOSED-EPISODE realized pnl (a data-gap episode always has
// OriginCensored — its opening buy is unseen). They do NOT partition ALL
// realized pnl: an open episode that already realized pnl via partial sells
// is not bucketed here — "closed pnl" means exactly closed-episode realized
// pnl, never the actor's total.
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
		// Entry evidence is the episode's FINAL lineage (v0.2.1.1): a
		// DataGap entry can never reach the research channel, exactly like
		// its episode. Strict also excludes gaps so the median channels'
		// N always equals EvidenceCounts.StrictN/ResearchN.
		strict := e.OriginQuality == storage.OriginConfirmedZero && !e.DataGap
		research := (e.OriginQuality == storage.OriginConfirmedZero ||
			e.OriginQuality == storage.OriginVisibleZero) && !e.DataGap
		if e.Initial {
			p.Entry.InitialCount++
			p.Entry.Initial.add(e.OriginQuality, e.DataGap)
			if strict {
				agesStrict = append(agesStrict, float64(e.ReceivedAt-e.TradeTime))
			}
			if research {
				agesResearch = append(agesResearch, float64(e.ReceivedAt-e.TradeTime))
			}
			if strict && e.HasChase {
				chasesStrict = append(chasesStrict, e.ChasePct)
			}
			if research && e.HasChase {
				chasesResearch = append(chasesResearch, e.ChasePct)
			}
			if strict && e.PriorValid {
				smartStrict = append(smartStrict, float64(e.SmartPrior))
				kolStrict = append(kolStrict, float64(e.KOLPrior))
			}
			if research && e.PriorValid {
				smartResearch = append(smartResearch, float64(e.SmartPrior))
				kolResearch = append(kolResearch, float64(e.KOLPrior))
			}
		} else {
			p.Entry.AddCount++
			p.Entry.Add.add(e.OriginQuality, e.DataGap)
			if strict && e.HasChase {
				addChasesStrict = append(addChasesStrict, e.ChasePct)
			}
			if research && e.HasChase {
				addChasesResearch = append(addChasesResearch, e.ChasePct)
			}
			if strict {
				sinceStrict = append(sinceStrict, float64(e.SinceInitialSecs))
			}
			if research {
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
	strictN, researchN := 0, 0
	reentryS, reentryR := 0, 0
	for _, e := range episodes {
		episodeN++
		p.Position.Evidence.add(e.OriginQuality, e.DataGap)
		strict := e.OriginQuality == storage.OriginConfirmedZero && !e.DataGap
		research := (e.OriginQuality == storage.OriginConfirmedZero ||
			e.OriginQuality == storage.OriginVisibleZero) && !e.DataGap
		if strict {
			strictN++
			if e.IsReentry {
				reentryS++
			}
			if e.Adds > 0 {
				addedStrict++
				addBuyStrict = append(addBuyStrict, e.AddBuyUSD)
			}
		}
		if research {
			researchN++
			if e.IsReentry {
				reentryR++
			}
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
		// ReentryRate = re-entry episodes / policy-eligible episodes, where
		// re-entry (IsReentry) is fixed at full-history reconstruction — the
		// evidence policy selects WHICH episodes enter the denominator, it
		// does not redefine who is a re-entry (a censored first episode
		// still makes the second a re-entry).
		p.Entry.ReentryRate = two(
			ratioStatN(reentryS, strictN),
			ratioStatN(reentryR, researchN),
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
	var addsS, addsR, redS, redR, holdS, holdR []float64
	for _, e := range episodes {
		strict := e.OriginQuality == storage.OriginConfirmedZero && !e.DataGap
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
		strict := e.OriginQuality == storage.OriginConfirmedZero && !e.DataGap
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
