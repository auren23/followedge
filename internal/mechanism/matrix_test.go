package mechanism

import (
	"math"
	"testing"

	"github.com/auren23/followedge/internal/storage"
)

// TestMatrixRowExtraction pins the contract the review demanded: labels are
// never part of the behavior feature vector, the features are the RESEARCH
// channel medians, and missingness (N=0) is tracked, never fabricated.
func TestMatrixRowExtraction(t *testing.T) {
	p := ActorBehaviorProfile{
		Entry: EntryStats{
			MedianChase:            TwoStat{Research: MedianStat{Value: 2.5, N: 10}},
			SmartPriorP50:          TwoStat{Research: MedianStat{Value: 1, N: 10}},
			MedianInitialBuy:       TwoStat{Research: MedianStat{Value: 500, N: 10}},
			AddEpisodeRate:         TwoStat{Research: MedianStat{Value: 0.6, N: 10}},
			MedianSinceInitialSecs: TwoStat{Research: MedianStat{Value: 60, N: 5}},
			MedianAddChase:         TwoStat{Research: MedianStat{Value: 4, N: 5}},
			ReentryRate:            TwoStat{Research: MedianStat{Value: 0.5, N: 10}},
		},
		Position: PositionStats{
			MedianHoldSecs: TwoStat{Research: MedianStat{Value: 7200, N: 10}},
		},
		Exit: ExitStats{
			PartialExitRatio: TwoStat{Research: MedianStat{Value: 0.5, N: 10}},
			FirstSellP50:     TwoStat{Research: MedianStat{Value: 120, N: 10}},
		},
	}
	row := MatrixRowFromProfile(p)
	if row.MissingFeatures != 0 {
		t.Errorf("missing features = %d, want 0 (all 10 research features present)", row.MissingFeatures)
	}
	if row.InitialChase.Value != 2.5 || row.PriorSmart.Value != 1 || row.AddDelay.Value != 60 ||
		row.AddChase.Value != 4 || row.HoldSecs.Value != 7200 || row.PartialExitRate.Value != 0.5 {
		t.Errorf("row features wrong: %+v", row)
	}

	// empty profile → every feature missing, counted once each
	row = MatrixRowFromProfile(ActorBehaviorProfile{})
	if row.MissingFeatures != 10 {
		t.Errorf("missing features on empty profile = %d, want 10", row.MissingFeatures)
	}
	if _, ok := row.feature(FeatureInitialChase); ok {
		t.Errorf("missing feature must not be present")
	}
}

// TestPatternMissingness pins that a missing feature never counts as "did
// not match": the pattern is unevaluable for that actor and excluded from
// the denominator (the matrix reports missingness separately).
func TestPatternMissingness(t *testing.T) {
	p := Pattern{Name: "x", Conditions: []FeatureCondition{{Feature: FeatureInitialChase, Op: "<=", A: 3}}}
	row := MechanismMatrixRow{InitialChase: MedianStat{}} // N=0 → missing
	if m, ok := p.matches(&row); ok || m {
		t.Errorf("missing feature must make the pattern unevaluable, got matched=%v ok=%v", m, ok)
	}

	// present but above threshold → evaluated, not matched
	row = MechanismMatrixRow{InitialChase: MedianStat{Value: 10, N: 5}}
	if m, ok := p.matches(&row); !ok || m {
		t.Errorf("present feature above threshold: want (matched=false, ok=true), got %v/%v", m, ok)
	}
}

// mkRow builds a matrix row with the five features of the "early independent
// entry + scaling" pattern fully present.
func mkRow(wallet string, prior, initChase, addRate, addDelay, addChase float64) MechanismMatrixRow {
	return MechanismMatrixRow{
		Wallet:         wallet,
		InitialChase:   MedianStat{Value: initChase, N: 5},
		PriorSmart:     MedianStat{Value: prior, N: 5},
		AddEpisodeRate: MedianStat{Value: addRate, N: 5},
		AddDelay:       MedianStat{Value: addDelay, N: 5},
		AddChase:       MedianStat{Value: addChase, N: 5},
		OriginCounts:   EvidenceCounts{Visible: 10},
	}
}

// TestEvaluatePatternsAndHypotheses mirrors the review example: a pattern at
// 11/15 target vs 3/18 control must graduate as an INFERRED / DISCOVERED
// hypothesis with auditable numbers; a pattern without prevalence separation
// must NOT graduate — the gate is the separation, not the story.
func TestEvaluatePatternsAndHypotheses(t *testing.T) {
	patterns := []Pattern{
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
			Name:       "momentum chase entry",
			Conditions: []FeatureCondition{{Feature: FeatureInitialChase, Op: ">", A: 5}},
		},
	}

	var target, control []MechanismMatrixRow
	// 15 target: 11 match the scaling pattern (chase 2, add chase 5),
	// 4 are momentum chasers (chase 8) → scaling 11/15, momentum 4/15.
	for i := 0; i < 11; i++ {
		target = append(target, mkRow(tWallet(i), 1, 2, 0.7, 60, 5))
	}
	for i := 11; i < 15; i++ {
		target = append(target, mkRow(tWallet(i), 1, 8, 0.2, 60, 9))
	}
	// 18 control: 3 happen to scale like the target, 9 momentum chasers,
	// 6 plain entries → scaling 3/18, momentum 9/18.
	for i := 0; i < 3; i++ {
		control = append(control, mkRow(cWallet(i), 1, 2, 0.7, 60, 5))
	}
	for i := 3; i < 12; i++ {
		control = append(control, mkRow(cWallet(i), 1, 8, 0.2, 60, 9))
	}
	for i := 12; i < 18; i++ {
		control = append(control, mkRow(cWallet(i), 1, 3, 0.4, 60, 4))
	}

	prevs := EvaluatePatterns(target, control, patterns)
	if len(prevs) != 2 {
		t.Fatalf("prevalence = %+v, want 2 patterns", prevs)
	}
	scaling := prevs[0]
	if scaling.TargetN != 15 || scaling.TargetHit != 11 || scaling.ControlN != 18 || scaling.ControlHit != 3 {
		t.Errorf("scaling prevalence = t %d/%d c %d/%d, want 11/15 3/18", scaling.TargetHit, scaling.TargetN, scaling.ControlHit, scaling.ControlN)
	}
	momentum := prevs[1]
	if momentum.TargetN != 15 || momentum.TargetHit != 4 {
		t.Errorf("momentum prevalence = t %d/%d, want 4/15", momentum.TargetHit, momentum.TargetN)
	}

	hyps := GenerateHypotheses(target, control, patterns, DefaultHypothesisOpts())
	if len(hyps) != 1 {
		t.Fatalf("hypotheses = %d, want 1 (only the separated pattern), got %+v", len(hyps), hyps)
	}
	h := hyps[0]
	if h.ID != "HYP-001" || h.Name != scaling.Name {
		t.Errorf("hypothesis = %s %q, want HYP-001 %q", h.ID, h.Name, scaling.Name)
	}
	if math.Abs(h.TargetSupport-11.0/15.0) > 0.001 || math.Abs(h.ControlSupport-3.0/18.0) > 0.001 {
		t.Errorf("support = t %.3f c %.3f, want 0.733/0.167", h.TargetSupport, h.ControlSupport)
	}
	if h.ActorN != 15 || h.EpisodeN != 150 { // 15 target rows × 10 research episodes
		t.Errorf("n = actors %d episodes %d, want 15/150", h.ActorN, h.EpisodeN)
	}
	if h.EvidenceLevel != EvidenceInferred || h.Status != HypothesisDiscovered {
		t.Errorf("hypothesis level/status = %s/%s, want INFERRED/DISCOVERED", h.EvidenceLevel, h.Status)
	}
	if len(h.SourceActors) != 11 {
		t.Errorf("source actors = %d, want 11 (the target actors that matched)", len(h.SourceActors))
	}
	// source actors sorted for a stable, auditable output
	for i := 1; i < len(h.SourceActors); i++ {
		if h.SourceActors[i] < h.SourceActors[i-1] {
			t.Errorf("source actors not sorted: %v", h.SourceActors)
		}
	}
	if len(h.Conditions) != 5 {
		t.Errorf("conditions = %d, want 5", len(h.Conditions))
	}
}

// TestGenerateHypothesesGates pins the gate semantics: no target sample, too
// little target prevalence, or no separation vs control — each alone kills
// the hypothesis.
func TestGenerateHypothesesGates(t *testing.T) {
	pattern := Pattern{Name: "x", Conditions: []FeatureCondition{{Feature: FeatureInitialChase, Op: "<=", A: 3}}}
	// 3 target actors all matching → below MinTargetN=5
	var target, control []MechanismMatrixRow
	for i := 0; i < 3; i++ {
		target = append(target, mkRow(tWallet(i), 1, 2, 0.7, 60, 5))
	}
	for i := 0; i < 18; i++ {
		control = append(control, mkRow(cWallet(i), 1, 2, 0.7, 60, 5)) // control also 100%
	}
	if hyps := GenerateHypotheses(target, control, []Pattern{pattern}, DefaultHypothesisOpts()); len(hyps) != 0 {
		t.Errorf("small target must not graduate: %+v", hyps)
	}

	// enough target but control matches equally → separation gate
	target = nil
	for i := 0; i < 10; i++ {
		target = append(target, mkRow(tWallet(i), 1, 2, 0.7, 60, 5))
	}
	if hyps := GenerateHypotheses(target, control, []Pattern{pattern}, DefaultHypothesisOpts()); len(hyps) != 0 {
		t.Errorf("no separation vs control must not graduate: %+v", hyps)
	}
}

// TestEvidenceCountsBuckets pins the mutually-exclusive provenance split and
// the strict/research definitions the matrix, CLI and hypotheses all share.
func TestEvidenceCountsBuckets(t *testing.T) {
	var c EvidenceCounts
	c.add(storage.OriginConfirmedZero, false)
	c.add(storage.OriginVisibleZero, false)
	c.add(storage.OriginCensored, false)
	c.add(storage.OriginVisibleZero, true) // gap wins over origin
	c.add(storage.OriginCensored, true)
	if c.Confirmed != 1 || c.Visible != 1 || c.Censored != 1 || c.DataGap != 2 {
		t.Errorf("buckets = %+v, want 1/1/1/2", c)
	}
	if c.StrictN() != 1 || c.ResearchN() != 2 {
		t.Errorf("strict/research = %d/%d, want 1/2", c.StrictN(), c.ResearchN())
	}
}

func tWallet(i int) string { return "TARGET_WALLET_" + string(rune('A'+i)) }
func cWallet(i int) string { return "CONTROL_WALLET_" + string(rune('A'+i)) }
