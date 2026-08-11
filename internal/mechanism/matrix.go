// Matrix and hypothesis contracts (v0.2.1.2 — research-integrity): the
// cross-actor mechanism matrix keeps OUTCOME LABELS strictly separate from
// BEHAVIOR FEATURES — Quality / ConsEV are labels, never clustering input —
// and produces AUDITABLE hypotheses by comparing outcome cells on
// hand-defined, interpretable patterns.
//
// The mechanism layer is OUTCOME-AGNOSTIC: it measures "does pattern P
// separate population X from population Y" (sideA vs sideB) with no
// knowledge of Quality/ConsEV. The analyze layer decides WHICH populations
// to compare (the Quality × Replicability 2×2: profit = A vs C, copyability
// = A vs B) and stamps the contrast name.
//
// Research-integrity gates (v0.2.1.2):
//   - MinSideBN: zero evaluable comparison actors can never graduate a
//     hypothesis — missing control evidence is NOT 0% prevalence (P0).
//   - Coverage gates: a pattern is only claimable where its features are
//     observable, on a representative fraction of each side's cell, and the
//     two sides' evaluability fractions must be comparable.
//   - Honest Ns: hypotheses expose total/evaluable/matched actor AND episode
//     counts per side; MatchedEpisodeN sums only the matched actors.
//
// Deliberately no KMeans: "11/15 target vs 3/18 control" is a hypothesis;
// "cluster #3 = swing traders" is a story.
package mechanism

import (
	"fmt"
	"math"
	"sort"

	"github.com/auren23/followedge/internal/storage"
)

// MechanismMatrixRow is one actor's row in the cross-actor matrix.
//
// Labels (Quality / ConsEV / EffectiveN / ReplCoverage / Trades) describe
// the OUTCOME — they never enter the behavior feature vector. The behavior
// features are the RESEARCH-channel medians of the behavior profile; N=0
// means the feature was NOT observable for this actor. Missingness is a
// first-class fact (MissingFeatures), never fabricated as zero.
type MechanismMatrixRow struct {
	Wallet     string
	WalletType string

	// outcome / labels — NOT part of the behavior feature vector
	Quality      float64
	ConsEV       float64
	EffectiveN   int
	ReplCoverage float64
	Trades       int

	// evidence coverage — how much of the actor's behavior is research-grade
	OriginCounts EvidenceCounts

	// research behavior features (research-channel medians; N=0 → missing)
	InitialChase    MedianStat
	PriorSmart      MedianStat
	InitialBuyUSD   MedianStat
	AddEpisodeRate  MedianStat
	AddDelay        MedianStat
	AddChase        MedianStat
	HoldSecs        MedianStat
	PartialExitRate MedianStat
	FirstSellSecs   MedianStat
	ReentryRate     MedianStat

	// missingness/coverage — how many behavior features had no research
	// evidence at all (out of the feature set above)
	MissingFeatures int
}

// Feature keys for FeatureCondition.
const (
	FeatureInitialChase    = "initial_chase"
	FeaturePriorSmart      = "prior_smart"
	FeatureInitialBuyUSD   = "initial_buy_usd"
	FeatureAddEpisodeRate  = "add_episode_rate"
	FeatureAddDelay        = "add_delay"
	FeatureAddChase        = "add_chase"
	FeatureHoldSecs        = "hold_secs"
	FeaturePartialExitRate = "partial_exit_rate"
	FeatureFirstSellSecs   = "first_sell_secs"
	FeatureReentryRate     = "reentry_rate"
)

// feature returns the named feature and whether it was observable (N>0).
func (r *MechanismMatrixRow) feature(name string) (MedianStat, bool) {
	switch name {
	case FeatureInitialChase:
		return r.InitialChase, r.InitialChase.N > 0
	case FeaturePriorSmart:
		return r.PriorSmart, r.PriorSmart.N > 0
	case FeatureInitialBuyUSD:
		return r.InitialBuyUSD, r.InitialBuyUSD.N > 0
	case FeatureAddEpisodeRate:
		return r.AddEpisodeRate, r.AddEpisodeRate.N > 0
	case FeatureAddDelay:
		return r.AddDelay, r.AddDelay.N > 0
	case FeatureAddChase:
		return r.AddChase, r.AddChase.N > 0
	case FeatureHoldSecs:
		return r.HoldSecs, r.HoldSecs.N > 0
	case FeaturePartialExitRate:
		return r.PartialExitRate, r.PartialExitRate.N > 0
	case FeatureFirstSellSecs:
		return r.FirstSellSecs, r.FirstSellSecs.N > 0
	case FeatureReentryRate:
		return r.ReentryRate, r.ReentryRate.N > 0
	}
	return MedianStat{}, false
}

// MatrixRowFromProfile extracts the RESEARCH-channel behavior features of a
// profile into a matrix row. Labels (Quality/ConsEV/...) are filled by the
// caller — the profile carries facts, not scores.
func MatrixRowFromProfile(p ActorBehaviorProfile) MechanismMatrixRow {
	row := MechanismMatrixRow{
		InitialChase:    p.Entry.MedianChase.Research,
		PriorSmart:      p.Entry.SmartPriorP50.Research,
		InitialBuyUSD:   p.Entry.MedianInitialBuy.Research,
		AddEpisodeRate:  p.Entry.AddEpisodeRate.Research,
		AddDelay:        p.Entry.MedianSinceInitialSecs.Research,
		AddChase:        p.Entry.MedianAddChase.Research,
		HoldSecs:        p.Position.MedianHoldSecs.Research,
		PartialExitRate: p.Exit.PartialExitRatio.Research,
		FirstSellSecs:   p.Exit.FirstSellP50.Research,
		ReentryRate:     p.Entry.ReentryRate.Research,
	}
	for _, f := range []MedianStat{
		row.InitialChase, row.PriorSmart, row.InitialBuyUSD, row.AddEpisodeRate,
		row.AddDelay, row.AddChase, row.HoldSecs, row.PartialExitRate,
		row.FirstSellSecs, row.ReentryRate,
	} {
		if f.N == 0 {
			row.MissingFeatures++
		}
	}
	return row
}

// CountEpisodeEvidence buckets episodes into the four mutually exclusive
// evidence buckets (a trajectory gap always wins).
func CountEpisodeEvidence(episodes []storage.Episode) EvidenceCounts {
	var c EvidenceCounts
	for _, e := range episodes {
		c.add(e.OriginQuality, e.DataGap)
	}
	return c
}

// FeatureCondition is one interpretable constraint on a matrix feature.
// Op: "<=", "<", ">=", ">", "between" (A..B), or "gt_feature" (A/B ignored,
// Other = the OTHER feature name — e.g. add_chase > initial_chase).
type FeatureCondition struct {
	Feature string
	Op      string
	A       float64
	B       float64 // upper bound for "between"
	Other   string  // other feature name for "gt_feature"
}

// matches reports (matched, evaluable). evaluable=false when a referenced
// feature is MISSING (N=0) — missingness must never count as "did not
// match": it is excluded from the denominator and tracked separately.
func (c FeatureCondition) matches(r *MechanismMatrixRow) (bool, bool) {
	if c.Op == "gt_feature" {
		a, okA := r.feature(c.Feature)
		b, okB := r.feature(c.Other)
		if !okA || !okB {
			return false, false
		}
		return a.Value > b.Value, true
	}
	f, ok := r.feature(c.Feature)
	if !ok {
		return false, false
	}
	switch c.Op {
	case "<=":
		return f.Value <= c.A, true
	case "<":
		return f.Value < c.A, true
	case ">=":
		return f.Value >= c.A, true
	case ">":
		return f.Value > c.A, true
	case "between":
		return f.Value >= c.A && f.Value <= c.B, true
	}
	return false, false
}

// Pattern is a named conjunction of conditions — an auditable candidate
// mechanism ("early independent entry + confirmed scaling"). Hand-defined;
// never derived from a clustering run.
type Pattern struct {
	Name       string
	Conditions []FeatureCondition
}

// matches reports (matched, evaluable) for the conjunction. One missing
// referenced feature makes the whole pattern unevaluable for this actor.
func (p Pattern) matches(r *MechanismMatrixRow) (bool, bool) {
	for _, c := range p.Conditions {
		m, ok := c.matches(r)
		if !ok {
			return false, false
		}
		if !m {
			return false, true
		}
	}
	return true, true
}

// evaluatePattern is the single walk shared by EvaluatePatterns and
// GenerateHypotheses: it returns the indices of rows where the pattern was
// evaluable and where it matched, so prevalence numbers and episode sums
// come from the SAME walk and can never disagree.
func evaluatePattern(p Pattern, rows []MechanismMatrixRow) (evaluable, matched []int) {
	for i := range rows {
		if m, ok := p.matches(&rows[i]); ok {
			evaluable = append(evaluable, i)
			if m {
				matched = append(matched, i)
			}
		}
	}
	return
}

// PatternPrevalence is one pattern's support in each side, WITH cohort
// totals (v0.2.1.2): the denominator is the evaluable count, but the total
// cohort size is carried so evaluability coverage is gated and visible.
type PatternPrevalence struct {
	Name string
	// SideATotal/SideBTotal are the side's cell size (band-matched population
	// — rows dropped by the activity/wallet band are excluded here and
	// reported separately).
	SideATotal int
	SideAN     int // rows where the pattern was evaluable
	SideAHit   int // evaluable rows that matched
	SideBTotal int
	SideBN     int
	SideBHit   int
}

// SideACoverage / SideBCoverage: the evaluable fraction of each side's cell.
func (p PatternPrevalence) SideACoverage() float64 { return coverage(p.SideAN, p.SideATotal) }
func (p PatternPrevalence) SideBCoverage() float64 { return coverage(p.SideBN, p.SideBTotal) }

// coverage: the evaluable fraction of a cohort. A cohort with zero rows has
// coverage 0 — nothing observable, never 100%, never NaN.
func coverage(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total)
}

// EvaluatePatterns measures every pattern's prevalence and evaluability in
// both sides.
func EvaluatePatterns(sideA, sideB []MechanismMatrixRow, patterns []Pattern) []PatternPrevalence {
	out := make([]PatternPrevalence, 0, len(patterns))
	for _, p := range patterns {
		pp := PatternPrevalence{Name: p.Name, SideATotal: len(sideA), SideBTotal: len(sideB)}
		evA, maA := evaluatePattern(p, sideA)
		pp.SideAN, pp.SideAHit = len(evA), len(maA)
		evB, maB := evaluatePattern(p, sideB)
		pp.SideBN, pp.SideBHit = len(evB), len(maB)
		out = append(out, pp)
	}
	return out
}

// EvidenceLevel is how trustworthy a hypothesis' claim is.
type EvidenceLevel string

const (
	EvidenceConfirmed   EvidenceLevel = "CONFIRMED"   // strict channel only
	EvidenceInferred    EvidenceLevel = "INFERRED"    // research channel (production default)
	EvidenceProvisional EvidenceLevel = "PROVISIONAL" // a hunch, not yet measured
)

// HypothesisStatus is the hypothesis' lifecycle state. v0.2.1.x only ever
// produces DISCOVERED; TESTING/SUPPORTED/REJECTED are for the replication
// milestone (holdout cohort / out-of-sample).
type HypothesisStatus string

const (
	HypothesisDiscovered HypothesisStatus = "DISCOVERED"
	HypothesisTesting    HypothesisStatus = "TESTING"
	HypothesisSupported  HypothesisStatus = "SUPPORTED"
	HypothesisRejected   HypothesisStatus = "REJECTED"
)

// SideSupport is one side's honest hypothesis evidence (v0.2.1.2): the
// three actor counts and the two episode counts. TotalN == the side's cell
// size; EvaluableEpisodeN / MatchedEpisodeN sum research episodes over
// evaluable / matched rows ONLY — cohort episodes of non-evaluable or
// non-matching actors are never hypothesis evidence.
type SideSupport struct {
	TotalN            int // all rows in the contrast side's cohort (cell)
	EvaluableN        int // rows where the pattern was evaluable
	MatchedN          int // evaluable rows that matched
	EvaluableEpisodeN int // Σ research episodes over EVALUABLE rows
	MatchedEpisodeN   int // Σ research episodes over MATCHED rows
}

// Prevalence is the matched fraction of the evaluable side (0 when nothing
// evaluable — never NaN, never a fabricated 0% claim on no data).
func (s SideSupport) Prevalence() float64 {
	if s.EvaluableN == 0 {
		return 0
	}
	return float64(s.MatchedN) / float64(s.EvaluableN)
}

// MechanismHypothesis is an AUDITABLE candidate mechanism: the exact
// conditions, both sides' full support, and the evidence level it is
// allowed to claim. No cluster names, no stories — only this.
//
// Contrast is stamped by the analyze layer ("profit" | "copyability");
// SourceActors are the side-A (discovery-side) matched actors. Hypotheses
// across the two contrasts share the cell-A discovery side and are NOT
// independent confirmations.
type MechanismHypothesis struct {
	ID              string
	Name            string
	Contrast        string
	EvidenceLevel   EvidenceLevel
	DiscoveryWindow string
	SideA           SideSupport
	SideB           SideSupport
	Conditions      []FeatureCondition
	SourceActors    []string // sorted, side-A matched actors
	Status          HypothesisStatus
}

// HypothesisOpts gates which patterns graduate to hypotheses. Two families:
// absolute EVALUABLE-actor floors (MinSideAN/MinSideBN) and relative
// evaluability-coverage floors on each side's cell (MinSideACoverage /
// MinSideBCoverage), plus the coverage-gap cap and the prevalence gates.
type HypothesisOpts struct {
	MinSideAN          int     // default 5 — min evaluable actors, discovery side
	MinSideBN          int     // default 5 — min evaluable actors, comparison side (the P0 gate)
	MinSideAPrevalence float64 // default 0.40 — min prevalence on the discovery side
	MinSeparation      float64 // default 0.25 — min prevalence separation vs the comparison side
	MinSideACoverage   float64 // default 0.50 — min evaluable fraction of side A's cell
	MinSideBCoverage   float64 // default 0.50 — min evaluable fraction of side B's cell
	MaxCoverageGap     float64 // default 0.30 — max |coverageA - coverageB|
	Window             string  // default "24h"
}

// DefaultHypothesisOpts: the evidential bar is identical for both contrasts
// (§5.13) — the owner's decision is to keep MinSideBN=5 for copyability too
// and wait for real data if cell B is scarce.
func DefaultHypothesisOpts() HypothesisOpts {
	return HypothesisOpts{
		MinSideAN:          5,
		MinSideBN:          5,
		MinSideAPrevalence: 0.40,
		MinSeparation:      0.25,
		MinSideACoverage:   0.50,
		MinSideBCoverage:   0.50,
		MaxCoverageGap:     0.30,
		Window:             "24h",
	}
}

// GateStatus evaluates the gate pipeline for one pattern prevalence, in
// short-circuit order. It returns pass and, when blocked, the FIRST failing
// gate's reason: "side-a-n", "side-b-n", "coverage-a", "coverage-b",
// "coverage-gap", "prevalence", "separation", or "OK".
//
// Order (does not change outcomes; only decides the reported reason):
//  1. SideAN >= MinSideAN AND SideBN >= MinSideBN        (P0 — no control evidence)
//  2. coverageA >= MinSideACoverage AND coverageB >= ... (evaluability floors)
//  3. |coverageA - coverageB| < MaxCoverageGap + 1e-9    (float-epsilon boundary)
//  4. pA >= MinSideAPrevalence AND pA - pB >= MinSeparation
func GateStatus(pp PatternPrevalence, opts HypothesisOpts) (bool, string) {
	if pp.SideAN < opts.MinSideAN || pp.SideBN < opts.MinSideBN {
		if pp.SideAN < opts.MinSideAN {
			return false, "side-a-n"
		}
		return false, "side-b-n"
	}
	covA, covB := pp.SideACoverage(), pp.SideBCoverage()
	if covA < opts.MinSideACoverage {
		return false, "coverage-a"
	}
	if covB < opts.MinSideBCoverage {
		return false, "coverage-b"
	}
	if math.Abs(covA-covB) >= opts.MaxCoverageGap+1e-9 {
		return false, "coverage-gap"
	}
	pA, pB := pp.SideAHit, pp.SideBHit
	if float64(pA) < opts.MinSideAPrevalence*float64(pp.SideAN) {
		return false, "prevalence"
	}
	// pA - pB >= MinSeparation  ⇔  pA*SideBN - pB*SideAN >= MinSeparation*SideAN*SideBN
	if float64(pA)*float64(pp.SideBN)-float64(pB)*float64(pp.SideAN) <
		opts.MinSeparation*float64(pp.SideAN)*float64(pp.SideBN) {
		return false, "separation"
	}
	return true, "OK"
}

// GenerateHypotheses turns evaluated patterns into auditable hypotheses:
// only patterns that clear ALL gates graduate. The evidence level is
// INFERRED (research channel; the strict channel is empty by design on the
// production path) and the status DISCOVERED — replication has not tested
// it yet. IDs are numbered per call (HYP-001…); the analyze layer renumbers
// globally across contrasts and stamps Contrast.
func GenerateHypotheses(sideA, sideB []MechanismMatrixRow, patterns []Pattern, opts HypothesisOpts) []MechanismHypothesis {
	byName := map[string]*Pattern{}
	for i := range patterns {
		byName[patterns[i].Name] = &patterns[i]
	}
	var out []MechanismHypothesis
	for _, pp := range EvaluatePatterns(sideA, sideB, patterns) {
		if pass, _ := GateStatus(pp, opts); !pass {
			continue
		}
		p := byName[pp.Name]
		evA, maA := evaluatePattern(*p, sideA)
		evalEpA, matchEpA := 0, 0
		for _, i := range evA {
			evalEpA += sideA[i].OriginCounts.ResearchN()
		}
		var src []string
		for _, i := range maA {
			matchEpA += sideA[i].OriginCounts.ResearchN()
			src = append(src, sideA[i].Wallet)
		}
		sort.Strings(src)
		evB, maB := evaluatePattern(*p, sideB)
		evalEpB, matchEpB := 0, 0
		for _, i := range evB {
			evalEpB += sideB[i].OriginCounts.ResearchN()
		}
		for _, i := range maB {
			matchEpB += sideB[i].OriginCounts.ResearchN()
		}
		out = append(out, MechanismHypothesis{
			ID:              fmt.Sprintf("HYP-%03d", len(out)+1),
			Name:            pp.Name,
			EvidenceLevel:   EvidenceInferred,
			DiscoveryWindow: opts.Window,
			SideA: SideSupport{
				TotalN: pp.SideATotal, EvaluableN: pp.SideAN, MatchedN: pp.SideAHit,
				EvaluableEpisodeN: evalEpA, MatchedEpisodeN: matchEpA,
			},
			SideB: SideSupport{
				TotalN: pp.SideBTotal, EvaluableN: pp.SideBN, MatchedN: pp.SideBHit,
				EvaluableEpisodeN: evalEpB, MatchedEpisodeN: matchEpB,
			},
			Conditions:   p.Conditions,
			SourceActors: src,
			Status:       HypothesisDiscovered,
		})
	}
	return out
}

// DefaultPatterns are the v0.2.1.x candidate mechanisms: hand-defined,
// interpretable, and compared side-vs-side. They are hypotheses to test,
// not clusters to name. add_chase > initial_chase is expressed as a
// cross-feature condition (gt_feature).
var DefaultPatterns = []Pattern{
	{
		Name: "early independent entry + consensus-confirmed scaling",
		Conditions: []FeatureCondition{
			{Feature: FeaturePriorSmart, Op: "<=", A: 1},
			{Feature: FeatureInitialChase, Op: "<=", A: 3},
			{Feature: FeatureAddEpisodeRate, Op: ">=", A: 0.6},
			{Feature: FeatureAddDelay, Op: "between", A: 30, B: 120},
			{Feature: FeatureAddChase, Op: "gt_feature", Other: FeatureInitialChase},
		},
	},
	{
		Name: "momentum chase entry",
		Conditions: []FeatureCondition{
			{Feature: FeatureInitialChase, Op: ">", A: 5},
		},
	},
	{
		Name: "confirmation-scale scalper",
		Conditions: []FeatureCondition{
			{Feature: FeatureAddEpisodeRate, Op: ">=", A: 0.6},
			{Feature: FeatureAddDelay, Op: "between", A: 30, B: 120},
			{Feature: FeaturePartialExitRate, Op: ">=", A: 0.5},
		},
	},
	{
		Name: "patient position trader",
		Conditions: []FeatureCondition{
			{Feature: FeatureAddEpisodeRate, Op: "<", A: 0.3},
			{Feature: FeatureHoldSecs, Op: ">=", A: 3600},
		},
	},
	{
		Name: "fast partial-exit swing",
		Conditions: []FeatureCondition{
			{Feature: FeaturePartialExitRate, Op: ">=", A: 0.5},
			{Feature: FeatureFirstSellSecs, Op: "<=", A: 300},
		},
	},
	{
		Name: "token revisitor",
		Conditions: []FeatureCondition{
			{Feature: FeatureReentryRate, Op: ">=", A: 0.5},
		},
	},
}
