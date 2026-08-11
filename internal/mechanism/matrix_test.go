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

var scalingPattern = Pattern{
	Name: "early independent entry + consensus-confirmed scaling",
	Conditions: []FeatureCondition{
		{Feature: FeaturePriorSmart, Op: "<=", A: 1},
		{Feature: FeatureInitialChase, Op: "<=", A: 3},
		{Feature: FeatureAddEpisodeRate, Op: ">=", A: 0.6},
		{Feature: FeatureAddDelay, Op: "between", A: 30, B: 120},
		{Feature: FeatureAddChase, Op: "gt_feature", Other: FeatureInitialChase},
	},
}

var momentumPattern = Pattern{
	Name:       "momentum chase entry",
	Conditions: []FeatureCondition{{Feature: FeatureInitialChase, Op: ">", A: 5}},
}

var addDelayPattern = Pattern{
	Name:       "adds quickly",
	Conditions: []FeatureCondition{{Feature: FeatureAddDelay, Op: "between", A: 30, B: 120}},
}

var scalperPattern = Pattern{
	Name: "confirmation-scale scalper",
	Conditions: []FeatureCondition{
		{Feature: FeatureAddEpisodeRate, Op: ">=", A: 0.6},
		{Feature: FeatureAddDelay, Op: "between", A: 30, B: 120},
		{Feature: FeaturePartialExitRate, Op: ">=", A: 0.5},
	},
}

// TestEvaluatePatternsAndHypotheses mirrors the review example: a pattern at
// 11/15 vs 3/18 must graduate as an INFERRED / DISCOVERED hypothesis with
// auditable per-side numbers; a pattern without prevalence separation must
// NOT graduate — the gate is the separation, not the story.
func TestEvaluatePatternsAndHypotheses(t *testing.T) {
	patterns := []Pattern{scalingPattern, momentumPattern}

	var sideA, sideB []MechanismMatrixRow
	// 15 side A: 11 match the scaling pattern (chase 2, add chase 5),
	// 4 are momentum chasers (chase 8) → scaling 11/15, momentum 4/15.
	for i := 0; i < 11; i++ {
		sideA = append(sideA, mkRow(tWallet(i), 1, 2, 0.7, 60, 5))
	}
	for i := 11; i < 15; i++ {
		sideA = append(sideA, mkRow(tWallet(i), 1, 8, 0.2, 60, 9))
	}
	// 18 side B: 3 happen to scale like side A, 9 momentum chasers,
	// 6 plain entries → scaling 3/18, momentum 9/18.
	for i := 0; i < 3; i++ {
		sideB = append(sideB, mkRow(cWallet(i), 1, 2, 0.7, 60, 5))
	}
	for i := 3; i < 12; i++ {
		sideB = append(sideB, mkRow(cWallet(i), 1, 8, 0.2, 60, 9))
	}
	for i := 12; i < 18; i++ {
		sideB = append(sideB, mkRow(cWallet(i), 1, 3, 0.4, 60, 4))
	}

	prevs := EvaluatePatterns(sideA, sideB, patterns)
	if len(prevs) != 2 {
		t.Fatalf("prevalence = %+v, want 2 patterns", prevs)
	}
	scaling := prevs[0]
	if scaling.SideATotal != 15 || scaling.SideAN != 15 || scaling.SideAHit != 11 ||
		scaling.SideBTotal != 18 || scaling.SideBN != 18 || scaling.SideBHit != 3 {
		t.Errorf("scaling prevalence = A %d/%d/%d B %d/%d/%d, want 15/15/11 18/18/3",
			scaling.SideATotal, scaling.SideAN, scaling.SideAHit,
			scaling.SideBTotal, scaling.SideBN, scaling.SideBHit)
	}
	if math.Abs(scaling.SideACoverage()-1.0) > 0.001 || math.Abs(scaling.SideBCoverage()-1.0) > 0.001 {
		t.Errorf("scaling coverage = A %.2f B %.2f, want 1.0/1.0", scaling.SideACoverage(), scaling.SideBCoverage())
	}
	momentum := prevs[1]
	if momentum.SideAN != 15 || momentum.SideAHit != 4 {
		t.Errorf("momentum prevalence = A %d/%d, want 4/15", momentum.SideAHit, momentum.SideAN)
	}

	hyps := GenerateHypotheses(sideA, sideB, patterns, DefaultHypothesisOpts())
	if len(hyps) != 1 {
		t.Fatalf("hypotheses = %d, want 1 (only the separated pattern), got %+v", len(hyps), hyps)
	}
	h := hyps[0]
	if h.ID != "HYP-001" || h.Name != scaling.Name {
		t.Errorf("hypothesis = %s %q, want HYP-001 %q", h.ID, h.Name, scaling.Name)
	}
	sa := h.SideA
	if sa.TotalN != 15 || sa.EvaluableN != 15 || sa.MatchedN != 11 ||
		sa.EvaluableEpisodeN != 150 || sa.MatchedEpisodeN != 110 {
		t.Errorf("side A support = %+v, want 15/15/11/150/110", sa)
	}
	if math.Abs(sa.Prevalence()-11.0/15.0) > 0.001 || math.Abs(h.SideB.Prevalence()-3.0/18.0) > 0.001 {
		t.Errorf("prevalence = A %.3f B %.3f, want 0.733/0.167", sa.Prevalence(), h.SideB.Prevalence())
	}
	if h.EvidenceLevel != EvidenceInferred || h.Status != HypothesisDiscovered {
		t.Errorf("hypothesis level/status = %s/%s, want INFERRED/DISCOVERED", h.EvidenceLevel, h.Status)
	}
	if len(h.SourceActors) != 11 {
		t.Errorf("source actors = %d, want 11 (the side-A actors that matched)", len(h.SourceActors))
	}
	for i := 1; i < len(h.SourceActors); i++ {
		if h.SourceActors[i] < h.SourceActors[i-1] {
			t.Errorf("source actors not sorted: %v", h.SourceActors)
		}
	}
	if len(h.Conditions) != 5 {
		t.Errorf("conditions = %d, want 5", len(h.Conditions))
	}
}

// TestGenerateHypothesesGates pins the gate semantics: too few evaluable
// actors on either side, too little prevalence, or no separation — each
// alone kills the hypothesis.
func TestGenerateHypothesesGates(t *testing.T) {
	pattern := Pattern{Name: "x", Conditions: []FeatureCondition{{Feature: FeatureInitialChase, Op: "<=", A: 3}}}
	// 3 side-A actors all matching → below MinSideAN=5
	var sideA, sideB []MechanismMatrixRow
	for i := 0; i < 3; i++ {
		sideA = append(sideA, mkRow(tWallet(i), 1, 2, 0.7, 60, 5))
	}
	for i := 0; i < 18; i++ {
		sideB = append(sideB, mkRow(cWallet(i), 1, 2, 0.7, 60, 5)) // side B also 100%
	}
	if hyps := GenerateHypotheses(sideA, sideB, []Pattern{pattern}, DefaultHypothesisOpts()); len(hyps) != 0 {
		t.Errorf("small side A must not graduate: %+v", hyps)
	}

	// enough side A but side B matches equally → separation gate
	sideA = nil
	for i := 0; i < 10; i++ {
		sideA = append(sideA, mkRow(tWallet(i), 1, 2, 0.7, 60, 5))
	}
	if hyps := GenerateHypotheses(sideA, sideB, []Pattern{pattern}, DefaultHypothesisOpts()); len(hyps) != 0 {
		t.Errorf("no separation vs side B must not graduate: %+v", hyps)
	}
}

// TestGateP0ControlNZero is trap T1a (mechanism half): zero evaluable
// comparison actors can NEVER graduate — missing control evidence is not 0%
// prevalence (the v0.2.1.1 bug that made target 10/10 vs unmeasured control
// graduate with Δ=100pp).
func TestGateP0ControlNZero(t *testing.T) {
	var sideA, sideB []MechanismMatrixRow
	for i := 0; i < 10; i++ {
		sideA = append(sideA, mkRow(tWallet(i), 1, 8, 0.2, 60, 9)) // all match momentum
	}
	for i := 0; i < 20; i++ {
		r := mkRow(cWallet(i), 1, 8, 0.2, 60, 9)
		r.InitialChase = MedianStat{} // feature missing → not evaluable
		sideB = append(sideB, r)
	}

	prevs := EvaluatePatterns(sideA, sideB, []Pattern{momentumPattern})
	if prevs[0].SideBN != 0 {
		t.Fatalf("side B evaluable = %d, want 0 (all features missing)", prevs[0].SideBN)
	}
	if pass, reason := GateStatus(prevs[0], DefaultHypothesisOpts()); pass || reason != "side-b-n" {
		t.Errorf("gate = %v/%s, want blocked by side-b-n", pass, reason)
	}
	if hyps := GenerateHypotheses(sideA, sideB, []Pattern{momentumPattern}, DefaultHypothesisOpts()); len(hyps) != 0 {
		t.Errorf("zero-evaluable control must NOT graduate, got %+v", hyps)
	}
}

// TestGateEmptySide is trap T2: an empty comparison side can never graduate
// (gate 1 side-b-n; also coverage(0,0)=0 fails gate 2 — both independently).
func TestGateEmptySide(t *testing.T) {
	var sideA []MechanismMatrixRow
	for i := 0; i < 10; i++ {
		sideA = append(sideA, mkRow(tWallet(i), 1, 8, 0.2, 60, 9))
	}
	pp := EvaluatePatterns(sideA, nil, []Pattern{momentumPattern})[0]
	if pp.SideBTotal != 0 || pp.SideBCoverage() != 0 {
		t.Errorf("empty side = total %d cov %.2f, want 0/0", pp.SideBTotal, pp.SideBCoverage())
	}
	if hyps := GenerateHypotheses(sideA, nil, []Pattern{momentumPattern}, DefaultHypothesisOpts()); len(hyps) != 0 {
		t.Errorf("empty side must not graduate: %+v", hyps)
	}
}

// TestGateCoverageFloor is trap T3: a pattern measured on an unrepresentative
// slice of a cohort must not graduate — 6/20 evaluable on side A is 30%
// coverage, below the 50% floor, even at 100% prevalence on the slice.
func TestGateCoverageFloor(t *testing.T) {
	var sideA, sideB []MechanismMatrixRow
	for i := 0; i < 6; i++ {
		sideA = append(sideA, mkRow(tWallet(i), 1, 2, 0.7, 60, 5))
	}
	for i := 6; i < 20; i++ {
		r := mkRow(tWallet(i), 1, 2, 0.7, 60, 5)
		r.AddDelay = MedianStat{} // never added → absent, not evaluable
		sideA = append(sideA, r)
	}
	for i := 0; i < 20; i++ {
		sideB = append(sideB, mkRow(cWallet(i), 1, 2, 0.7, 200, 5)) // evaluable, none match
	}
	pp := EvaluatePatterns(sideA, sideB, []Pattern{addDelayPattern})[0]
	if pp.SideAN != 6 || math.Abs(pp.SideACoverage()-0.30) > 0.001 {
		t.Fatalf("side A = n %d cov %.2f, want 6/0.30", pp.SideAN, pp.SideACoverage())
	}
	if pass, reason := GateStatus(pp, DefaultHypothesisOpts()); pass || reason != "coverage-a" {
		t.Errorf("gate = %v/%s, want blocked by coverage-a", pass, reason)
	}
	if hyps := GenerateHypotheses(sideA, sideB, []Pattern{addDelayPattern}, DefaultHypothesisOpts()); len(hyps) != 0 {
		t.Errorf("14/20 unevaluable side must not graduate: %+v", hyps)
	}
}

// TestGateCoverageGap is trap T4: when the evaluability gap between the
// sides IS the only separation, the hypothesis must not graduate; and the
// exact 0.30 boundary is a defined pass (float epsilon), not noise.
func TestGateCoverageGap(t *testing.T) {
	var sideA, sideB []MechanismMatrixRow
	for i := 0; i < 10; i++ {
		sideA = append(sideA, mkRow(tWallet(i), 1, 2, 0.7, 60, 5)) // cov 100%, 8 match
	}
	for i := 0; i < 5; i++ {
		sideB = append(sideB, mkRow(cWallet(i), 1, 10, 0.2, 60, 12)) // evaluable, no match
	}
	for i := 5; i < 10; i++ {
		r := mkRow(cWallet(i), 1, 10, 0.2, 60, 12)
		r.InitialChase = MedianStat{} // missing → cov 50%
		sideB = append(sideB, r)
	}
	pp := EvaluatePatterns(sideA, sideB, []Pattern{momentumPattern})[0]
	if math.Abs(pp.SideACoverage()-1.0) > 0.001 || math.Abs(pp.SideBCoverage()-0.50) > 0.001 {
		t.Fatalf("coverage = A %.2f B %.2f, want 1.0/0.5", pp.SideACoverage(), pp.SideBCoverage())
	}
	if pass, reason := GateStatus(pp, DefaultHypothesisOpts()); pass || reason != "coverage-gap" {
		t.Errorf("gap 0.5 must block via coverage-gap, got %v/%s", pass, reason)
	}

	// Boundary pin (float epsilon): gap exactly 0.30 (0.8-0.5) passes when
	// the coverage floors and N floors are relaxed; a 0.35 gap blocks.
	relaxed := HypothesisOpts{MinSideACoverage: 0.4, MinSideBCoverage: 0.4, MaxCoverageGap: 0.30}
	boundary := PatternPrevalence{SideATotal: 10, SideAN: 8, SideAHit: 8, SideBTotal: 10, SideBN: 5, SideBHit: 0}
	if pass, _ := GateStatus(boundary, relaxed); !pass {
		t.Errorf("0.8-0.5 = 0.30000000000000004 must PASS at cap 0.30 (epsilon)")
	}
	over := PatternPrevalence{SideATotal: 10, SideAN: 10, SideAHit: 8, SideBTotal: 10, SideBN: 5, SideBHit: 0}
	over.SideATotal, over.SideAN, over.SideAHit = 20, 17, 17 // cov 0.85 vs 0.5 → gap 0.35
	if pass, reason := GateStatus(over, relaxed); pass || reason != "coverage-gap" {
		t.Errorf("0.85-0.5 = 0.35 must BLOCK, got %v/%s", pass, reason)
	}
}

// TestGateEmptyTotal is trap T5a: coverage(0,0) == 0 — an empty cohort is
// not evaluable, never 100%, never NaN.
func TestGateEmptyTotal(t *testing.T) {
	pp := PatternPrevalence{SideATotal: 8, SideAN: 8, SideAHit: 8, SideBTotal: 0, SideBN: 0}
	if pp.SideBCoverage() != 0 {
		t.Errorf("coverage(0,0) = %.2f, want 0", pp.SideBCoverage())
	}
	// an empty side also fails gate 1 first (side-b-n)
	if pass, reason := GateStatus(pp, DefaultHypothesisOpts()); pass || reason != "side-b-n" {
		t.Errorf("gate = %v/%s, want blocked by side-b-n", pass, reason)
	}
}

// TestContrastIndependence is trap T7 (mechanism half): the SAME pattern must
// graduate under the profit pairing (A vs C) but not the copyability pairing
// (A vs B) — the two contrasts answer different questions. The old blended
// control graduated both with a claim answerable to neither.
func TestContrastIndependence(t *testing.T) {
	mk3 := func(wallet string, addRate, addDelay, partial float64) MechanismMatrixRow {
		r := MechanismMatrixRow{
			Wallet:          wallet,
			AddEpisodeRate:  MedianStat{Value: addRate, N: 5},
			AddDelay:        MedianStat{Value: addDelay, N: 5},
			PartialExitRate: MedianStat{Value: partial, N: 5},
			OriginCounts:    EvidenceCounts{Visible: 10},
		}
		return r
	}
	// "confirmation-scale scalper" is common in high-quality cells, rare in
	// low-quality ones: A 4/5, B 4/5, C 1/5, D 1/5.
	var a, b, c []MechanismMatrixRow
	for i := 0; i < 4; i++ {
		a = append(a, mk3(tWallet(i), 0.7, 60, 0.6))
		c = append(c, mk3(cWallet(i), 0.2, 200, 0.2))
		b = append(b, mk3(tWallet(10+i), 0.7, 60, 0.6))
	}
	a = append(a, mk3(tWallet(9), 0.2, 200, 0.2))
	c = append(c, mk3(cWallet(9), 0.7, 60, 0.6))
	b = append(b, mk3(tWallet(14), 0.2, 200, 0.2))

	patterns := []Pattern{scalperPattern}
	if hyps := GenerateHypotheses(a, c, patterns, DefaultHypothesisOpts()); len(hyps) != 1 {
		t.Errorf("profit (A vs C): 80%% vs 20%% must graduate, got %d", len(hyps))
	}
	if hyps := GenerateHypotheses(a, b, patterns, DefaultHypothesisOpts()); len(hyps) != 0 {
		t.Errorf("copyability (A vs B): 80%% vs 80%% must NOT graduate, got %d", len(hyps))
	}
}

// TestHonestHypothesisNs is trap T8: episode sums count only evaluable /
// matched actors — never the whole cohort. The pinned variant makes every
// expected value unique so a regression over the wrong rows cannot slip
// through a coincidental match.
func TestHonestHypothesisNs(t *testing.T) {
	pattern := Pattern{Name: "x", Conditions: []FeatureCondition{{Feature: FeatureInitialChase, Op: "<=", A: 3}}}
	var sideA, sideB []MechanismMatrixRow
	for i := 0; i < 11; i++ {
		sideA = append(sideA, mkRow(tWallet(i), 1, 2, 0.7, 60, 5)) // matches
	}
	for i := 11; i < 15; i++ {
		sideA = append(sideA, mkRow(tWallet(i), 1, 8, 0.2, 60, 9)) // evaluable, no match
	}
	for i := 0; i < 15; i++ {
		sideB = append(sideB, mkRow(cWallet(i), 1, 10, 0.2, 60, 12)) // evaluable, no match
	}
	hyps := GenerateHypotheses(sideA, sideB, []Pattern{pattern}, DefaultHypothesisOpts())
	if len(hyps) != 1 {
		t.Fatalf("hypotheses = %d, want 1", len(hyps))
	}
	sa := hyps[0].SideA
	if sa.TotalN != 15 || sa.EvaluableN != 15 || sa.MatchedN != 11 ||
		sa.EvaluableEpisodeN != 150 || sa.MatchedEpisodeN != 110 {
		t.Errorf("side A = %+v, want 15/15/11/150/110", sa)
	}

	// Variant (pinned): 3 of the 15 side-A rows lose their feature, and 2 of
	// those 3 were among the 11 matches → MatchedN=9, MatchedEpisodeN=90.
	var sideA2 []MechanismMatrixRow
	for i := 0; i < 9; i++ {
		sideA2 = append(sideA2, mkRow(tWallet(i), 1, 2, 0.7, 60, 5))
	}
	for i := 9; i < 12; i++ {
		sideA2 = append(sideA2, mkRow(tWallet(i), 1, 8, 0.2, 60, 9))
	}
	for i := 12; i < 15; i++ {
		r := mkRow(tWallet(i), 1, 2, 0.7, 60, 5) // 2 of these were matches
		r.InitialChase = MedianStat{}
		sideA2 = append(sideA2, r)
	}
	hyps = GenerateHypotheses(sideA2, sideB, []Pattern{pattern}, DefaultHypothesisOpts())
	if len(hyps) != 1 {
		t.Fatalf("variant hypotheses = %d, want 1", len(hyps))
	}
	sa = hyps[0].SideA
	if sa.TotalN != 15 || sa.EvaluableN != 12 || sa.MatchedN != 9 ||
		sa.EvaluableEpisodeN != 120 || sa.MatchedEpisodeN != 90 {
		t.Errorf("variant side A = %+v, want 15/12/9/120/90", sa)
	}
	if sa.EvaluableEpisodeN == 150 || sa.MatchedEpisodeN == 150 {
		t.Errorf("episode sums must not equal the old inflated 150: %+v", sa)
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
