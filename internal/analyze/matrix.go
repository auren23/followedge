package analyze

import (
	"fmt"
	"io"
	"math"
	"sort"
	"time"

	"github.com/auren23/followedge/internal/mechanism"
	"github.com/auren23/followedge/internal/storage"
)

// Matrix builds the v0.2.1.2 cross-actor mechanism matrix: one row per
// actor with replication evidence (labels from the rank census, behavior
// features from the RESEARCH channel of the behavior profile), split into
// the Quality × Replicability 2×2 outcome cells, and compared under two
// first-class contrasts:
//
//	Profit      A vs C — both ConsEV > 0, differ in actor quality
//	Copyability A vs B — both quality-high, differ in follower ConsEV
//
// Labels (Quality/ConsEV/EffectiveN/...) never enter the behavior feature
// vector. Every pattern prevalence is produced under BOTH contrasts with
// coverage gates and honest per-side Ns; hypotheses graduate only when
// both sides are measured and comparable (P0: zero evaluable comparison
// actors can never graduate).
func Matrix(w io.Writer, s *storage.Store, since time.Time, horizon, grace time.Duration,
	noExitLoss float64, clusterWindow time.Duration, minReplMarket, minReplFilled int,
	minQuality float64, opts mechanism.HypothesisOpts) error {

	groups, err := s.ActorGroups(since)
	if err != nil {
		return err
	}
	actors := actorRows(groups)
	census, err := s.ReplicationCensus(since, horizon, grace, time.Now().UTC())
	if err != nil {
		return err
	}
	for _, r := range census {
		if a := actors[r.Wallet]; a != nil {
			attachReplication(a, &r, noExitLoss)
		}
	}

	replEligible := func(a *Actor) bool {
		return a.ConsValid && a.Filled+a.MarketLoss >= minReplMarket && a.Filled >= minReplFilled
	}

	// behavior features per actor — one unified reconstruction pass each
	// (entries carry their episode's final evidence, so episode stats and
	// entry stats agree by construction).
	wallets := make([]string, 0, len(actors))
	for wallet := range actors {
		wallets = append(wallets, wallet)
	}
	sort.Strings(wallets)

	var rows []mechanism.MechanismMatrixRow
	excluded := 0
	for _, wallet := range wallets {
		a := actors[wallet]
		if !replEligible(a) {
			excluded++
			continue
		}
		row, err := matrixRowFor(s, a, since, clusterWindow)
		if err != nil {
			fmt.Fprintf(w, "(matrix: %s skipped: %v)\n", shortAddr(wallet), err)
			excluded++
			continue
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		fmt.Fprintf(w, "MECHANISM MATRIX — no actors with replication evidence yet (need effective n >= %d, filled >= %d). Keep collecting.\n",
			minReplMarket, minReplFilled)
		return nil
	}

	a, b, c, d, dropped, bandLo, bandHi, bandNote := splitCells(rows, minQuality)

	fmt.Fprintf(w, "MECHANISM MATRIX (since %s, horizon %v, quality gate %.0f)\n",
		since.Format("2006-01-02"), horizon, minQuality)
	if bandNote != "" {
		fmt.Fprintf(w, "  %s\n", bandNote)
	} else {
		fmt.Fprintf(w, "  matching: activity band %d-%d trades (cell A raw median) · wallet types %s\n",
			bandLo, bandHi, typeSummary(typesOf(a)))
	}
	if excluded > 0 {
		fmt.Fprintf(w, "  (%d actors excluded: no replication evidence or below the sample gate)\n", excluded)
	}
	if dropped > 0 {
		fmt.Fprintf(w, "  (%d actors dropped: outside the cell-A activity/wallet band)\n", dropped)
	}

	printOutcome2x2(w, a, b, c, d)
	printCellCoverage(w, a, b, c, d)
	printCellFeatures(w, a, b, c, d)

	// ---- contrasts (both always run) ----
	patterns := mechanism.DefaultPatterns
	profitHyps := runContrast(w, "PROFIT", "C", "profit", a, c, patterns, opts)
	for i := range profitHyps {
		profitHyps[i].ID = fmt.Sprintf("HYP-%03d", i+1)
		profitHyps[i].Contrast = "profit"
	}
	printHypothesisBlock(w, "PROFIT (A vs C)", profitHyps, a, c, patterns, opts)

	copyHyps := runContrast(w, "COPYABILITY", "B", "copyability", a, b, patterns, opts)
	for i := range copyHyps {
		copyHyps[i].ID = fmt.Sprintf("HYP-%03d", len(profitHyps)+i+1)
		copyHyps[i].Contrast = "copyability"
	}
	printHypothesisBlock(w, "COPYABILITY (A vs B)", copyHyps, a, b, patterns, opts)

	// ---- footer notes (multiplicity + precision + next step) ----
	fmt.Fprintf(w, "\n  next step: replication cohort (ConsEV > 0, effective n >= 20) — these are DISCOVERED, not SUPPORTED\n")
	fmt.Fprintf(w, "  note: %d patterns × 2 contrasts = %d screenings per window, re-run on overlapping cohorts —\n",
		len(patterns), len(patterns)*2)
	fmt.Fprintf(w, "        DISCOVERED is a screened candidate list, not a finding.\n")
	fmt.Fprintf(w, "  note: at per-side N = %d–15 and prevalence near 0.5, SE(Δ) ranges roughly ±18–30pp (worst ~±30pp\n",
		opts.MinSideAN)
	fmt.Fprintf(w, "        at the gates' floors); a Δ below ~2 SE of zero is not distinguishable from noise —\n")
	fmt.Fprintf(w, "        treat the numbers as candidates, not estimates; formal inference is deferred to the\n")
	fmt.Fprintf(w, "        replication milestone.\n")
	return nil
}

// matrixRowFor reconstructs one actor's behavior over FULL history (episodes
// filtered to the window cohort), builds the profile, and extracts the
// research-channel features + labels.
func matrixRowFor(s *storage.Store, a *Actor, since time.Time, clusterWindow time.Duration) (mechanism.MechanismMatrixRow, error) {
	ds, err := s.ReconstructBehaviorFor(a.Wallet)
	if err != nil {
		return mechanism.MechanismMatrixRow{}, err
	}
	episodes := ds.Episodes[:0]
	for _, e := range ds.Episodes {
		if e.OpenedAt >= since.Unix() {
			episodes = append(episodes, e)
		}
	}
	obs, err := s.EntryObservations(a.Wallet, since)
	if err != nil {
		return mechanism.MechanismMatrixRow{}, err
	}
	chaseByEvent := map[string]float64{}
	for _, o := range obs {
		chaseByEvent[o.EventID] = o.ChasePct
	}
	datasetStart, err := s.DatasetStart()
	if err != nil {
		return mechanism.MechanismMatrixRow{}, err
	}
	var facts []mechanism.EntryFact
	for _, ce := range ds.Entries {
		if !inCohort(ce, since.Unix()) {
			continue
		}
		f := mechanism.EntryFact{
			Initial: ce.Initial, TradeTime: ce.TradeTime, ReceivedAt: ce.ReceivedAt,
			SinceInitialSecs: ce.SinceInitialSecs, OriginQuality: ce.OriginQuality,
			DataGap: ce.DataGap,
		}
		if ch, ok := chaseByEvent[ce.EventID]; ok {
			f.ChasePct, f.HasChase = ch, true
		}
		if ce.Initial {
			pf, err := s.PriorFlowAt(ce.Token, ce.TradeTime, clusterWindow, datasetStart)
			if err != nil {
				return mechanism.MechanismMatrixRow{}, err
			}
			f.SmartPrior, f.KOLPrior, f.PriorValid = pf.Smart, pf.KOL, pf.Valid
		}
		facts = append(facts, f)
	}
	row := mechanism.MatrixRowFromProfile(mechanism.BuildProfile(a.Wallet, episodes, facts))
	row.Wallet = a.Wallet
	row.WalletType = a.WalletType
	row.Quality = a.Quality
	row.ConsEV = a.ConsEV
	row.EffectiveN = a.Filled + a.MarketLoss
	row.ReplCoverage = pct(a.Filled, a.Due) / 100
	row.Trades = a.Trades
	row.OriginCounts = mechanism.CountEpisodeEvidence(episodes)
	return row, nil
}

// splitCells partitions repl-eligible rows into the Quality × Replicability
// 2×2 outcome cells (v0.2.1.2, band applied to ALL four cells in
// v0.2.1.3):
//
//	              ConsEV > 0      ConsEV <= 0
//	Quality >= g   A (target)     B
//	Quality <  g   C              D
//
// The activity band (trades within [0.5, 2] × the RAW A median) and the
// wallet-type set are derived from cell A's candidates, then applied in ONE
// pass to EVERY row — cells differ in OUTCOME, not in who they are, and
// cell A is band-matched exactly like B/C/D (an A-label outlier can never
// confound the contrasts with an activity difference). Rows outside the
// band are dropped and counted. If cell A has no candidates the band cannot
// be derived: rows are bucketed by labels only and a note is returned (no
// contrast can run — both involve A, but the 2×2 layout stays printable as
// the diagnostic).
func splitCells(rows []mechanism.MechanismMatrixRow, minQuality float64) (a, b, c, d []mechanism.MechanismMatrixRow, dropped int, bandLo, bandHi int, bandNote string) {
	// Pass 1: rawA = all A-LABEL rows (unfiltered) — the band's derivation
	// population. Only the median and type set of rawA matter; A itself is
	// band-filtered in pass 2 like every other cell.
	var rawA []mechanism.MechanismMatrixRow
	for _, r := range rows {
		if r.ConsEV > 0 && r.Quality >= minQuality {
			rawA = append(rawA, r)
		}
	}
	if len(rawA) == 0 {
		for _, r := range rows {
			switch {
			case r.Quality >= minQuality:
				b = append(b, r)
			case r.ConsEV > 0:
				c = append(c, r)
			default:
				d = append(d, r)
			}
		}
		return a, b, c, d, 0, 0, 0, "(no cell A — activity band unavailable; all rows bucketed by label)"
	}
	var trades []int
	types := map[string]bool{}
	for _, r := range rawA {
		trades = append(trades, r.Trades)
		types[r.WalletType] = true
	}
	sort.Ints(trades)
	med := trades[len(trades)/2]
	bandLo, bandHi = int(float64(med)*0.5), int(float64(med)*2.0)

	// Pass 2: ONE band-filtered pass over ALL rows — A/B/C/D all come from
	// the same matched population.
	for _, r := range rows {
		if r.Trades < bandLo || r.Trades > bandHi || !types[r.WalletType] {
			dropped++
			continue
		}
		switch {
		case r.ConsEV > 0 && r.Quality >= minQuality:
			a = append(a, r)
		case r.Quality >= minQuality:
			b = append(b, r)
		case r.ConsEV > 0:
			c = append(c, r)
		default:
			d = append(d, r)
		}
	}
	return a, b, c, d, dropped, bandLo, bandHi, ""
}

// runContrast prints one contrast's header + PATTERNS table and returns the
// hypotheses that cleared the gates (IDs per-call; the caller renumbers
// globally and stamps the contrast). Empty side → not-evaluable note, no
// pattern table, no hypotheses (the empty-side reporting rule).
func runContrast(w io.Writer, name, emptyCell, contrast string,
	sideA, sideB []mechanism.MechanismMatrixRow, patterns []mechanism.Pattern,
	opts mechanism.HypothesisOpts) []mechanism.MechanismHypothesis {

	if len(sideA) == 0 || len(sideB) == 0 {
		cell := emptyCell
		if len(sideA) == 0 {
			cell = "A"
		}
		fmt.Fprintf(w, "\nCONTRAST: %s — not evaluable\n", name)
		fmt.Fprintf(w, "  (contrast not evaluable: cell %s is empty — 0 rows matched the A band)\n", cell)
		return nil
	}
	desc := ""
	switch contrast {
	case "profit":
		desc = "both cells ConsEV > 0; the question is what separates strong from weak actors"
	case "copyability":
		desc = "both cells quality >= gate; the question is what separates the copyable edge"
	}
	fmt.Fprintf(w, "\nCONTRAST: %s — %s\n", name, desc)

	// data-missing is pattern-independent: rows with NO research episodes
	// (nothing measurable for any pattern). absent is per-pattern.
	dataMissingA := countDataMissing(sideA)
	dataMissingB := countDataMissing(sideB)
	labels := map[string]string{}
	for _, p := range patterns {
		labels[p.Name] = absentLabelFor(p)
	}

	fmt.Fprintf(w, "\nPATTERNS — prevalence per side\n")
	fmt.Fprintf(w, "  (canonical per side: matched M/E · evaluable E/T · total T · cov P%% · un-eval U/V: N data · M label)\n")
	fmt.Fprintf(w, "  %-44s %-52s   %-52s   %6s  %s\n", "pattern", "side A", "side B", "Δ", "gate")
	for _, pp := range mechanism.EvaluatePatterns(sideA, sideB, patterns) {
		label := labels[pp.Name]
		cellA := sideCellStr(pp.SideATotal, pp.SideAN, pp.SideAHit, dataMissingA, label, 0, 0, false)
		cellB := sideCellStr(pp.SideBTotal, pp.SideBN, pp.SideBHit, dataMissingB, label, 0, 0, false)
		pass, reason := mechanism.GateStatus(pp, opts)
		gate := "OK"
		if !pass {
			gate = reason
		}
		delta := "-"
		if pp.SideAN > 0 && pp.SideBN > 0 {
			d := float64(pp.SideAHit)/float64(pp.SideAN) - float64(pp.SideBHit)/float64(pp.SideBN)
			delta = fmt.Sprintf("%+.0fpp", d*100)
		}
		fmt.Fprintf(w, "  %-44s %-52s   %-52s   %6s  %s\n", pp.Name, cellA, cellB, delta, gate)
	}
	return mechanism.GenerateHypotheses(sideA, sideB, patterns, opts)
}

func countDataMissing(rows []mechanism.MechanismMatrixRow) int {
	n := 0
	for _, r := range rows {
		if r.OriginCounts.ResearchN() == 0 {
			n++
		}
	}
	return n
}

// absentLabelFor decides the un-eval bucket label for a pattern: the
// bucket's internal name is "absent", NEVER printed verbatim. Patterns
// referencing only add-family features (add_delay, add_chase — the only
// class whose N=0 can genuinely mean "the behavior never occurred") print
// "no-add-evidence"; every other pattern prints "unobservable", so a
// coverage-blocked chase pattern can never read as "cell C doesn't chase".
func absentLabelFor(p mechanism.Pattern) string {
	addFamily := map[string]bool{mechanism.FeatureAddDelay: true, mechanism.FeatureAddChase: true}
	for _, c := range p.Conditions {
		if c.Op == "gt_feature" && !addFamily[c.Other] {
			return "unobservable"
		}
		if !addFamily[c.Feature] {
			return "unobservable"
		}
	}
	return "no-add-evidence"
}

// sideCellStr renders the canonical per-side format shared by the PATTERNS
// table and the HYPOTHESES block: "matched M/E · evaluable E/T · total T",
// then context fields. withEpisodes renders "· episodes X/Y" (the REAL
// evaluable/matched episode sums — never cohort totals); otherwise "· cov
// P%". When coverage < 100% the un-eval split with its label is appended.
func sideCellStr(total, n, hit, dataMissing int, label string, evalEp, matchEp int, withEpisodes bool) string {
	s := fmt.Sprintf("matched %d/%d · evaluable %d/%d · total %d", hit, n, n, total, total)
	if withEpisodes {
		s += fmt.Sprintf(" · episodes %d/%d", evalEp, matchEp)
	}
	if n < total {
		absent := total - n - dataMissing
		cov := 0
		if total > 0 {
			cov = int(math.Round(float64(n) / float64(total) * 100))
		}
		s += fmt.Sprintf(" · cov %d%% (un-eval %d/%d: %d data · %d %s)", cov, total-n, total, dataMissing, absent, label)
	} else if !withEpisodes {
		s += " · cov 100%"
	}
	return s
}

func printOutcome2x2(w io.Writer, a, b, c, d []mechanism.MechanismMatrixRow) {
	fmt.Fprintf(w, "\nOUTCOME 2×2 (matched population — every cell respects the activity/wallet band)\n")
	fmt.Fprintf(w, "                ConsEV > 0     ConsEV <= 0\n")
	fmt.Fprintf(w, "  quality high  A %6d      B %6d\n", len(a), len(b))
	fmt.Fprintf(w, "  quality low   C %6d      D %6d\n", len(c), len(d))
}

func printCellCoverage(w io.Writer, a, b, c, d []mechanism.MechanismMatrixRow) {
	fmt.Fprintf(w, "\nEVIDENCE COVERAGE (episodes per cell)\n")
	fmt.Fprintf(w, "  cell  n    confirmed  visible  censored  gap  research\n")
	for _, cell := range []struct {
		name string
		rows []mechanism.MechanismMatrixRow
	}{{"A", a}, {"B", b}, {"C", c}, {"D", d}} {
		var ec mechanism.EvidenceCounts
		for _, r := range cell.rows {
			ec.Confirmed += r.OriginCounts.Confirmed
			ec.Visible += r.OriginCounts.Visible
			ec.Censored += r.OriginCounts.Censored
			ec.DataGap += r.OriginCounts.DataGap
		}
		fmt.Fprintf(w, "  %-4s %-4d %10d %9d %9d %5d %9d\n",
			cell.name, len(cell.rows), ec.Confirmed, ec.Visible, ec.Censored, ec.DataGap, ec.ResearchN())
	}
}

func printCellFeatures(w io.Writer, a, b, c, d []mechanism.MechanismMatrixRow) {
	fmt.Fprintf(w, "\nFEATURES (research medians per cell — N in parens)\n")
	fmt.Fprintf(w, "  %-18s %14s %14s %14s %14s\n", "feature", "A", "B", "C", "D")
	for _, f := range matrixFeatures {
		fmt.Fprintf(w, "  %-18s %14s %14s %14s %14s\n", f.label,
			cellFeature(f, a), cellFeature(f, b), cellFeature(f, c), cellFeature(f, d))
	}
}

func cellFeature(f featureInfo, rows []mechanism.MechanismMatrixRow) string {
	var vals []float64
	for _, r := range rows {
		if m := f.get(r); m.N > 0 {
			vals = append(vals, m.Value)
		}
	}
	if len(vals) == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%s (%d)", fmtFeature(median(vals), f.unit), len(vals))
}

func printHypothesisBlock(w io.Writer, name string, hyps []mechanism.MechanismHypothesis,
	sideA, sideB []mechanism.MechanismMatrixRow, patterns []mechanism.Pattern, opts mechanism.HypothesisOpts) {
	fmt.Fprintf(w, "\nHYPOTHESES — %s · %s · %s (research channel, not yet replicated)\n",
		name, mechanism.HypothesisDiscovered, mechanism.EvidenceInferred)
	if len(hyps) == 0 {
		fmt.Fprintf(w, "  (no pattern cleared the gates: side n >= %d each, coverage >= %.0f%% each, |Δ coverage| <= %.0fpp, prevalence >= %.0f%%, separation >= %.0fpp)\n",
			opts.MinSideAN, opts.MinSideACoverage*100, opts.MaxCoverageGap*100,
			opts.MinSideAPrevalence*100, opts.MinSeparation*100)
		fmt.Fprintf(w, "  (the cov / un-eval columns above show which patterns were coverage-blocked; override any gate with\n")
		fmt.Fprintf(w, "   --min-side-a-n --min-side-b-n --min-side-a-coverage --min-side-b-coverage --max-coverage-gap --min-prevalence --min-separation)\n")
		return
	}
	dmA, dmB := countDataMissing(sideA), countDataMissing(sideB)
	labels := map[string]string{}
	for _, p := range patterns {
		labels[p.Name] = absentLabelFor(p)
	}
	for _, h := range hyps {
		label := labels[h.Name]
		fmt.Fprintf(w, "\n  %s  %s\n", h.ID, h.Name)
		fmt.Fprintf(w, "    A: %s\n", sideCellStr(h.SideA.TotalN, h.SideA.EvaluableN, h.SideA.MatchedN, dmA, label,
			h.SideA.EvaluableEpisodeN, h.SideA.MatchedEpisodeN, true))
		fmt.Fprintf(w, "    B: %s\n", sideCellStr(h.SideB.TotalN, h.SideB.EvaluableN, h.SideB.MatchedN, dmB, label,
			h.SideB.EvaluableEpisodeN, h.SideB.MatchedEpisodeN, true))
		fmt.Fprintf(w, "    Δ %+.0fpp · conditions: %s\n", (h.SideA.Prevalence()-h.SideB.Prevalence())*100, condString(h.Conditions))
		if len(h.SourceActors) > 0 {
			fmt.Fprintf(w, "    source actors (A): %s\n", joinWallets(h.SourceActors))
		}
	}
}

// featureInfo and matrixFeatures drive the per-cell FEATURES table.
type featureInfo struct {
	key   string
	label string
	unit  string // "pct" | "usd" | "secs" | "num"
	get   func(mechanism.MechanismMatrixRow) mechanism.MedianStat
}

var matrixFeatures = []featureInfo{
	{mechanism.FeatureInitialChase, "initial chase", "pct", func(r mechanism.MechanismMatrixRow) mechanism.MedianStat { return r.InitialChase }},
	{mechanism.FeaturePriorSmart, "prior smart P50", "num", func(r mechanism.MechanismMatrixRow) mechanism.MedianStat { return r.PriorSmart }},
	{mechanism.FeatureInitialBuyUSD, "initial buy", "usd", func(r mechanism.MechanismMatrixRow) mechanism.MedianStat { return r.InitialBuyUSD }},
	{mechanism.FeatureAddEpisodeRate, "add episode rate", "pct", func(r mechanism.MechanismMatrixRow) mechanism.MedianStat { return r.AddEpisodeRate }},
	{mechanism.FeatureAddDelay, "add delay", "secs", func(r mechanism.MechanismMatrixRow) mechanism.MedianStat { return r.AddDelay }},
	{mechanism.FeatureAddChase, "add chase", "pct", func(r mechanism.MechanismMatrixRow) mechanism.MedianStat { return r.AddChase }},
	{mechanism.FeatureHoldSecs, "hold", "secs", func(r mechanism.MechanismMatrixRow) mechanism.MedianStat { return r.HoldSecs }},
	{mechanism.FeaturePartialExitRate, "partial exit rate", "pct", func(r mechanism.MechanismMatrixRow) mechanism.MedianStat { return r.PartialExitRate }},
	{mechanism.FeatureFirstSellSecs, "first sell", "secs", func(r mechanism.MechanismMatrixRow) mechanism.MedianStat { return r.FirstSellSecs }},
	{mechanism.FeatureReentryRate, "reentry rate", "pct", func(r mechanism.MechanismMatrixRow) mechanism.MedianStat { return r.ReentryRate }},
}

func fmtFeature(v float64, unit string) string {
	switch unit {
	case "usd":
		return fmt.Sprintf("$%.0f", v)
	case "secs":
		return fmt.Sprintf("%.0fs", v)
	case "pct":
		return fmt.Sprintf("%.1f%%", v)
	default:
		return fmt.Sprintf("%.1f", v)
	}
}

func condString(conds []mechanism.FeatureCondition) string {
	out := ""
	for i, c := range conds {
		if i > 0 {
			out += " · "
		}
		switch c.Op {
		case "gt_feature":
			out += fmt.Sprintf("%s > %s", c.Feature, c.Other)
		case "between":
			out += fmt.Sprintf("%s %.0f-%.0f", c.Feature, c.A, c.B)
		default:
			out += fmt.Sprintf("%s %s %g", c.Feature, c.Op, c.A)
		}
	}
	return out
}

func joinWallets(ws []string) string {
	out := ""
	for i, w := range ws {
		if i > 0 {
			out += " "
		}
		out += shortAddr(w)
	}
	return out
}

func shortAddr(a string) string {
	if len(a) <= 12 {
		return a
	}
	return a[:6] + "..." + a[len(a)-4:]
}

func typesOf(rows []mechanism.MechanismMatrixRow) map[string]int {
	types := map[string]int{}
	for _, r := range rows {
		types[r.WalletType]++
	}
	return types
}

func typeSummary(types map[string]int) string {
	keys := make([]string, 0, len(types))
	for k := range types {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := ""
	for i, k := range keys {
		if i > 0 {
			out += " "
		}
		out += fmt.Sprintf("%s×%d", k, types[k])
	}
	return out
}
