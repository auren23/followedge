package analyze

import (
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/auren23/followedge/internal/mechanism"
	"github.com/auren23/followedge/internal/storage"
)

// Matrix builds the v0.2.1.1 cross-actor mechanism matrix: one row per
// actor with replication evidence (labels from the rank census, behavior
// features from the RESEARCH channel of the behavior profile), splits a
// TARGET cohort (profitable + copyable) against a CONTROL cohort (same
// source/window, similar activity, but ConsEV <= 0 or low quality), and
// reports pattern prevalence + generated hypotheses.
//
// Labels (Quality/ConsEV/EffectiveN/...) never enter the behavior feature
// vector: the question is whether a BEHAVIOR pattern separates the cohorts,
// not whether the cohorts separate on their own labels.
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

	// ---- cohort split ----
	target, control := splitCohorts(rows, minQuality)
	excluded += len(rows) - len(target) - len(control)

	fmt.Fprintf(w, "MECHANISM MATRIX (since %s, horizon %v, quality gate %.0f)\n",
		since.Format("2006-01-02"), horizon, minQuality)
	fmt.Fprintf(w, "  target  = profitable + copyable: ConsEV > 0, quality >= %.0f, effective-n/coverage gate passed\n", minQuality)
	fmt.Fprintf(w, "  control = same source & window, similar activity, but ConsEV <= 0 / below quality gate\n")
	if excluded > 0 {
		fmt.Fprintf(w, "  (%d actors excluded: no replication evidence or below the sample gate)\n", excluded)
	}

	printCohorts(w, target, control)

	// ---- evidence coverage ----
	printEvidenceCoverage(w, target, control)

	// ---- features ----
	printFeatureComparison(w, target, control)

	// ---- patterns + hypotheses ----
	patterns := mechanism.DefaultPatterns
	printPatterns(w, target, control, patterns)
	printHypotheses(w, target, control, patterns, opts)
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

// splitCohorts assigns repl-eligible rows to TARGET / CONTROL. Control is
// banded to the target's activity (trade count within [0.5, 2]× of the
// target median, wallet type from the target's set) — the two cohorts must
// differ in OUTCOME, not in who they are.
func splitCohorts(rows []mechanism.MechanismMatrixRow, minQuality float64) (target, control []mechanism.MechanismMatrixRow) {
	for _, r := range rows {
		if r.ConsEV > 0 && r.Quality >= minQuality {
			target = append(target, r)
		}
	}
	if len(target) == 0 {
		return target, nil
	}
	var targetTrades []int
	targetTypes := map[string]bool{}
	for _, r := range target {
		targetTrades = append(targetTrades, r.Trades)
		targetTypes[r.WalletType] = true
	}
	sort.Ints(targetTrades)
	med := targetTrades[len(targetTrades)/2]
	lo, hi := float64(med)*0.5, float64(med)*2.0
	for _, r := range rows {
		if r.ConsEV > 0 && r.Quality >= minQuality {
			continue // already target
		}
		if float64(r.Trades) < lo || float64(r.Trades) > hi {
			continue // activity band: not comparable to the target
		}
		if !targetTypes[r.WalletType] {
			continue // wallet-type band: same kinds of actors
		}
		control = append(control, r)
	}
	return target, control
}

func printCohorts(w io.Writer, target, control []mechanism.MechanismMatrixRow) {
	medTrades := func(rs []mechanism.MechanismMatrixRow) (int, string) {
		if len(rs) == 0 {
			return 0, "-"
		}
		vals := make([]int, 0, len(rs))
		types := map[string]int{}
		for _, r := range rs {
			vals = append(vals, r.Trades)
			types[r.WalletType]++
		}
		sort.Ints(vals)
		return vals[len(vals)/2], typeSummary(types)
	}
	medN := func(rs []mechanism.MechanismMatrixRow) (int, int) {
		if len(rs) == 0 {
			return 0, 0
		}
		vals := make([]int, 0, len(rs))
		cov := make([]float64, 0, len(rs))
		for _, r := range rs {
			vals = append(vals, r.EffectiveN)
			cov = append(cov, r.ReplCoverage)
		}
		sort.Ints(vals)
		sort.Float64s(cov)
		return vals[len(vals)/2], int(cov[len(cov)/2] * 100)
	}
	fmt.Fprintf(w, "\nCOHORTS\n")
	fmt.Fprintf(w, "  %-8s %4s %12s  %-18s %14s %12s\n", "cohort", "n", "trades(med)", "wallet types", "effN(med)", "cov(med)")
	tr, tt := medTrades(target)
	tn, tc := medN(target)
	fmt.Fprintf(w, "  %-8s %4d %12d  %-18s %14d %11d%%\n", "TARGET", len(target), tr, tt, tn, tc)
	cr, ct := medTrades(control)
	cn, cc := medN(control)
	fmt.Fprintf(w, "  %-8s %4d %12d  %-18s %14d %11d%%\n", "CONTROL", len(control), cr, ct, cn, cc)
	if len(control) == 0 {
		fmt.Fprintf(w, "  (no control cohort matched the activity band — prevalence comparisons below are one-sided)\n")
	}
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

func printEvidenceCoverage(w io.Writer, target, control []mechanism.MechanismMatrixRow) {
	sum := func(rs []mechanism.MechanismMatrixRow) mechanism.EvidenceCounts {
		var c mechanism.EvidenceCounts
		for _, r := range rs {
			c.Confirmed += r.OriginCounts.Confirmed
			c.Visible += r.OriginCounts.Visible
			c.Censored += r.OriginCounts.Censored
			c.DataGap += r.OriginCounts.DataGap
		}
		return c
	}
	t, c := sum(target), sum(control)
	fmt.Fprintf(w, "\nEVIDENCE COVERAGE (cohort episodes)\n")
	fmt.Fprintf(w, "  %-8s %9s %9s %9s %9s %9s\n", "cohort", "confirmed", "inferred", "censored", "gap", "researchN")
	fmt.Fprintf(w, "  %-8s %9d %9d %9d %9d %9d\n", "TARGET", t.Confirmed, t.Visible, t.Censored, t.DataGap, t.ResearchN())
	fmt.Fprintf(w, "  %-8s %9d %9d %9d %9d %9d\n", "CONTROL", c.Confirmed, c.Visible, c.Censored, c.DataGap, c.ResearchN())
}

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

func printFeatureComparison(w io.Writer, target, control []mechanism.MechanismMatrixRow) {
	fmt.Fprintf(w, "\nFEATURES (research channel — cohort medians of per-actor medians)\n")
	fmt.Fprintf(w, "  %-20s %-18s %-18s %s\n", "feature", "target", "control", "present t/c")
	coh := func(rs []mechanism.MechanismMatrixRow, get func(mechanism.MechanismMatrixRow) mechanism.MedianStat) (mechanism.MedianStat, int) {
		var vals []float64
		for _, r := range rs {
			if m := get(r); m.N > 0 {
				vals = append(vals, m.Value)
			}
		}
		if len(vals) == 0 {
			return mechanism.MedianStat{}, 0
		}
		return mechanism.MedianStat{Value: median(vals), N: len(vals)}, len(vals)
	}
	for _, f := range matrixFeatures {
		tm, tn := coh(target, f.get)
		cm, cn := coh(control, f.get)
		tStr := "n/a"
		if tn > 0 {
			tStr = fmt.Sprintf("%s (%d)", fmtFeature(tm.Value, f.unit), tn)
		}
		cStr := "n/a"
		if cn > 0 {
			cStr = fmt.Sprintf("%s (%d)", fmtFeature(cm.Value, f.unit), cn)
		}
		fmt.Fprintf(w, "  %-20s %-18s %-18s %d/%d · %d/%d\n", f.label, tStr, cStr, tn, len(target), cn, len(control))
	}
}

func printPatterns(w io.Writer, target, control []mechanism.MechanismMatrixRow, patterns []mechanism.Pattern) {
	fmt.Fprintf(w, "\nPATTERNS — prevalence (matched / evaluable)\n")
	fmt.Fprintf(w, "  %-52s %-16s %-16s %s\n", "pattern", "target", "control", "Δ")
	for _, pp := range mechanism.EvaluatePatterns(target, control, patterns) {
		tStr, cStr := "n/a", "n/a"
		delta := "-"
		if pp.TargetN > 0 {
			tSup := float64(pp.TargetHit) / float64(pp.TargetN)
			tStr = fmt.Sprintf("%d/%d %.0f%%", pp.TargetHit, pp.TargetN, tSup*100)
		}
		if pp.ControlN > 0 {
			cSup := float64(pp.ControlHit) / float64(pp.ControlN)
			cStr = fmt.Sprintf("%d/%d %.0f%%", pp.ControlHit, pp.ControlN, cSup*100)
			if pp.TargetN > 0 {
				delta = fmt.Sprintf("%+.0fpp", (float64(pp.TargetHit)/float64(pp.TargetN)-cSup)*100)
			}
		}
		fmt.Fprintf(w, "  %-52s %-16s %-16s %s\n", pp.Name, tStr, cStr, delta)
	}
}

func printHypotheses(w io.Writer, target, control []mechanism.MechanismMatrixRow, patterns []mechanism.Pattern, opts mechanism.HypothesisOpts) {
	hyps := mechanism.GenerateHypotheses(target, control, patterns, opts)
	fmt.Fprintf(w, "\nHYPOTHESES (%s · %s — research channel, not yet replicated)\n",
		mechanism.HypothesisDiscovered, mechanism.EvidenceInferred)
	if len(hyps) == 0 {
		fmt.Fprintf(w, "  (no pattern cleared the gates: target n >= %d, target prevalence >= %.0f%%, separation >= %.0fpp)\n",
			opts.MinTargetN, opts.MinTargetPrevalence*100, opts.MinSeparation*100)
		return
	}
	for _, h := range hyps {
		fmt.Fprintf(w, "\n  %s  %s\n", h.ID, h.Name)
		fmt.Fprintf(w, "    target %d/%d (%.0f%%) · control %.0f%% · episodes %d\n",
			int(h.TargetSupport*float64(h.ActorN)), h.ActorN, h.TargetSupport*100,
			h.ControlSupport*100, h.EpisodeN)
		fmt.Fprintf(w, "    conditions: %s\n", condString(h.Conditions))
		if len(h.SourceActors) > 0 {
			fmt.Fprintf(w, "    source actors: %s\n", joinWallets(h.SourceActors))
		}
	}
	fmt.Fprintf(w, "\n  next step: replication cohort (ConsEV > 0, effective n >= %d) — these are DISCOVERED, not SUPPORTED\n", 20)
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
