// Matrix and hypothesis contracts (v0.2.1.1): the cross-actor mechanism
// matrix keeps OUTCOME LABELS strictly separate from BEHAVIOR FEATURES —
// Quality / ConsEV are labels, never clustering input — and produces
// AUDITABLE hypotheses by comparing a TARGET cohort against a CONTROL
// cohort on hand-defined, interpretable patterns.
//
// Deliberately no KMeans: a cluster's story is only as good as the
// prevalence separation behind it. "11/15 target vs 3/18 control" is a
// hypothesis; "cluster #3 = swing traders" is a story.
package mechanism

import (
	"fmt"
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

// PatternPrevalence is one pattern's support in each cohort (denominators
// exclude actors whose referenced features were missing).
type PatternPrevalence struct {
	Name       string
	TargetN    int
	TargetHit  int
	ControlN   int
	ControlHit int
}

// EvaluatePatterns measures every pattern's prevalence in both cohorts.
func EvaluatePatterns(target, control []MechanismMatrixRow, patterns []Pattern) []PatternPrevalence {
	out := make([]PatternPrevalence, 0, len(patterns))
	for _, p := range patterns {
		pp := PatternPrevalence{Name: p.Name}
		for _, r := range target {
			if m, ok := p.matches(&r); ok {
				pp.TargetN++
				if m {
					pp.TargetHit++
				}
			}
		}
		for _, r := range control {
			if m, ok := p.matches(&r); ok {
				pp.ControlN++
				if m {
					pp.ControlHit++
				}
			}
		}
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

// HypothesisStatus is the hypothesis' lifecycle state. v0.2.1.1 only ever
// produces DISCOVERED; TESTING/SUPPORTED/REJECTED are for the replication
// milestone (holdout cohort / out-of-sample).
type HypothesisStatus string

const (
	HypothesisDiscovered HypothesisStatus = "DISCOVERED"
	HypothesisTesting    HypothesisStatus = "TESTING"
	HypothesisSupported  HypothesisStatus = "SUPPORTED"
	HypothesisRejected   HypothesisStatus = "REJECTED"
)

// MechanismHypothesis is an AUDITABLE candidate mechanism: the exact
// conditions, the target/control prevalence it came from, and the evidence
// level it is allowed to claim. No cluster names, no stories — only this.
type MechanismHypothesis struct {
	ID              string
	Name            string
	EvidenceLevel   EvidenceLevel
	DiscoveryWindow string
	ActorN          int     // target actors the pattern was evaluated on
	EpisodeN        int     // research episodes across those target actors
	TargetSupport   float64 // hit / evaluable target actors
	ControlSupport  float64
	Conditions      []FeatureCondition
	SourceActors    []string
	Status          HypothesisStatus
}

// HypothesisOpts gates which patterns graduate to hypotheses: minimum
// target sample, minimum target prevalence, and minimum prevalence
// separation vs control.
type HypothesisOpts struct {
	MinTargetN          int
	MinTargetPrevalence float64
	MinSeparation       float64
	Window              string
}

// DefaultHypothesisOpts mirrors the review example (11/15 vs 3/18 needs
// separation of ~0.57; 0.25 is a lenient floor for the first pass).
func DefaultHypothesisOpts() HypothesisOpts {
	return HypothesisOpts{
		MinTargetN:          5,
		MinTargetPrevalence: 0.40,
		MinSeparation:       0.25,
		Window:              "24h",
	}
}

// GenerateHypotheses turns evaluated patterns into auditable hypotheses:
// only patterns with enough target support AND a real prevalence separation
// vs control graduate. The evidence level is INFERRED (research channel;
// the strict channel is empty by design on the production path) and the
// status DISCOVERED — replication has not tested it yet.
func GenerateHypotheses(target, control []MechanismMatrixRow, patterns []Pattern, opts HypothesisOpts) []MechanismHypothesis {
	byName := map[string]*Pattern{}
	for i := range patterns {
		byName[patterns[i].Name] = &patterns[i]
	}
	var out []MechanismHypothesis
	for _, pp := range EvaluatePatterns(target, control, patterns) {
		if pp.TargetN < opts.MinTargetN {
			continue
		}
		tSup := float64(pp.TargetHit) / float64(pp.TargetN)
		cSup := 0.0
		if pp.ControlN > 0 {
			cSup = float64(pp.ControlHit) / float64(pp.ControlN)
		}
		if tSup < opts.MinTargetPrevalence || tSup-cSup < opts.MinSeparation {
			continue
		}
		p := byName[pp.Name]
		episodes := 0
		var src []string
		for _, r := range target {
			episodes += r.OriginCounts.ResearchN()
			if m, ok := p.matches(&r); ok && m {
				src = append(src, r.Wallet)
			}
		}
		sort.Strings(src)
		out = append(out, MechanismHypothesis{
			ID:              fmt.Sprintf("HYP-%03d", len(out)+1),
			Name:            pp.Name,
			EvidenceLevel:   EvidenceInferred,
			DiscoveryWindow: opts.Window,
			ActorN:          pp.TargetN,
			EpisodeN:        episodes,
			TargetSupport:   tSup,
			ControlSupport:  cSup,
			Conditions:      p.Conditions,
			SourceActors:    src,
			Status:          HypothesisDiscovered,
		})
	}
	return out
}

// DefaultPatterns are the v0.2.1.1 candidate mechanisms: hand-defined,
// interpretable, and compared target-vs-control. They are hypotheses to
// test, not clusters to name. add_chase > initial_chase is expressed as a
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
