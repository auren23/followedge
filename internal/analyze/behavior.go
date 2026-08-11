package analyze

import (
	"fmt"
	"io"
	"time"

	"github.com/auren23/followedge/internal/mechanism"
	"github.com/auren23/followedge/internal/storage"
)

// Behavior prints one actor's full behavior reconstruction card (v0.2.0):
// QUALITY + REPLICATION (the v0.1 facts) followed by the BEHAVIOR profile —
// entry context, position management, exit behavior. Facts only; archetype
// classification and hypothesis generation come in later milestones.
//
// Point-in-time: entry-context features (prior flow, chase) only use data
// knowable at each entry's trade_time. Episodes are reconstructed on demand
// over the wallet's FULL history (left-truncation guard), then filtered to
// the analysis window.
func Behavior(w io.Writer, s *storage.Store, wallet string, since time.Time,
	horizon time.Duration, noExitLoss float64, grace time.Duration, clusterWindow time.Duration) error {
	// ---- QUALITY (same window) ----
	groups, err := s.ActorGroups(since)
	if err != nil {
		return err
	}
	var filtered []storage.ActorGroup
	for _, g := range groups {
		if g.Wallet == wallet {
			filtered = append(filtered, g)
		}
	}
	if len(filtered) == 0 {
		return fmt.Errorf("no trades for %s in window", wallet)
	}
	actors := actorRows(filtered)
	a := actors[wallet]

	// ---- REPLICATION (same window, buy-side census) ----
	var repl *storage.ReplicationRow
	census, err := s.ReplicationCensus(since, horizon, grace, time.Now().UTC())
	if err != nil {
		return err
	}
	for i := range census {
		if census[i].Wallet == wallet {
			repl = &census[i]
			break
		}
	}
	attachReplication(a, repl, noExitLoss)

	fmt.Fprintf(w, "ACTOR %s (%s)\n", wallet, a.WalletType)
	fmt.Fprintf(w, "\nQUALITY\n")
	fmt.Fprintf(w, "  %.1f\n", a.Quality)
	fmt.Fprintf(w, "  trades %d · pnl $%.0f · consist %.0f%%\n",
		a.Trades, a.RealizedPnL, pct(a.ProfitableDays, a.ActiveDays))
	if repl != nil {
		consStr := "n/a"
		market := repl.Filled + repl.MarketLoss
		if market > 0 {
			cons := (repl.ObservedEV*float64(repl.Filled) - float64(repl.MarketLoss)*noExitLoss) / float64(market)
			consStr = fmt.Sprintf("%+.1f%%", cons)
		}
		obsStr := "-"
		if repl.ObservedValid {
			obsStr = fmt.Sprintf("%+.1f%%", repl.ObservedEV)
		}
		fmt.Fprintf(w, "\nREPLICATION @%v\n", horizon)
		fmt.Fprintf(w, "  effective n    %d\n", repl.Filled+repl.MarketLoss)
		fmt.Fprintf(w, "  coverage       %.0f%%\n", pct(repl.Filled, repl.Due))
		fmt.Fprintf(w, "  obs EV         %s\n", obsStr)
		fmt.Fprintf(w, "  cons EV        %s\n", consStr)
	}

	// ---- BEHAVIOR (full-history reconstruction, filtered to window) ----
	// Single unified walker (v0.2.1.1): episodes AND classified entries come
	// from one reconstruction pass, and entries carry their episode's FINAL
	// evidence — they can never claim research eligibility the episode lost.
	ds, err := s.ReconstructBehaviorFor(wallet)
	if err != nil {
		return err
	}
	episodes := ds.Episodes[:0]
	for _, e := range ds.Episodes {
		if e.OpenedAt >= since.Unix() {
			episodes = append(episodes, e)
		}
	}
	classified := ds.Entries
	obs, err := s.EntryObservations(wallet, since)
	if err != nil {
		return err
	}
	chaseByEvent := map[string]float64{}
	for _, o := range obs {
		chaseByEvent[o.EventID] = o.ChasePct
	}
	datasetStart, err := s.DatasetStart()
	if err != nil {
		return err
	}
	// PIT prior flow per entry (initial entries only; adds get their own
	// since-initial context). Cohort rule: an entry belongs to the profile iff
	// its EPISODE opened in the window — adds are judged by their opening
	// time, not their own trade time, so the card's counts always describe
	// the same set of positions.
	var facts []mechanism.EntryFact
	for _, ce := range classified {
		if !inCohort(ce, since.Unix()) {
			continue
		}
		f := mechanism.EntryFact{
			Initial:          ce.Initial,
			TradeTime:        ce.TradeTime,
			ReceivedAt:       ce.ReceivedAt,
			SinceInitialSecs: ce.SinceInitialSecs,
			OriginQuality:    ce.OriginQuality,
			DataGap:          ce.DataGap,
		}
		if ch, ok := chaseByEvent[ce.EventID]; ok {
			f.ChasePct, f.HasChase = ch, true
		}
		if ce.Initial {
			pf, err := s.PriorFlowAt(ce.Token, ce.TradeTime, clusterWindow, datasetStart)
			if err != nil {
				return err
			}
			f.SmartPrior, f.KOLPrior, f.PriorValid = pf.Smart, pf.KOL, pf.Valid
		}
		facts = append(facts, f)
	}

	prof := mechanism.BuildProfile(wallet, episodes, facts)

	fmt.Fprintf(w, "\nBEHAVIOR COHORT — positions opened since %s\n", since.Format("2006-01-02"))
	fmt.Fprintf(w, "\n                         confirmed   research\n")
	fmt.Fprintf(w, "\nENTRY\n")
	fmt.Fprintf(w, "  initial buys        %-11s %-11s  confirmed %d · visible %d · censored %d · gap %d\n",
		fmt.Sprintf("%d", prof.Entry.Initial.StrictN()),
		fmt.Sprintf("%d", prof.Entry.Initial.ResearchN()),
		prof.Entry.Initial.Confirmed, prof.Entry.Initial.Visible,
		prof.Entry.Initial.Censored, prof.Entry.Initial.DataGap)
	fmt.Fprintf(w, "  add buys            %-11s %-11s  confirmed %d · visible %d · censored %d · gap %d\n",
		fmt.Sprintf("%d", prof.Entry.Add.StrictN()),
		fmt.Sprintf("%d", prof.Entry.Add.ResearchN()),
		prof.Entry.Add.Confirmed, prof.Entry.Add.Visible,
		prof.Entry.Add.Censored, prof.Entry.Add.DataGap)
	fmt.Fprintf(w, "  reentry rate        %s\n", twoPct(prof.Entry.ReentryRate))
	fmt.Fprintf(w, "  initial buy         %s\n", twoUsd(prof.Entry.MedianInitialBuy))
	fmt.Fprintf(w, "  add buy             %s\n", twoUsd(prof.Entry.MedianAddBuy))
	fmt.Fprintf(w, "  add episode rate    %s\n", twoPct(prof.Entry.AddEpisodeRate))
	fmt.Fprintf(w, "  total capital       %s\n", twoUsd(prof.Entry.MedianCapitalIn))
	fmt.Fprintf(w, "  median age          %s\n", twoSecs(prof.Entry.MedianAge))
	fmt.Fprintf(w, "  median chase        %s\n", twoPct(prof.Entry.MedianChase))
	fmt.Fprintf(w, "  median add chase    %s\n", twoPct(prof.Entry.MedianAddChase))
	fmt.Fprintf(w, "  add since open      %s\n", twoSecs(prof.Entry.MedianSinceInitialSecs))
	fmt.Fprintf(w, "  prior smart P50     %s\n", twoNum(prof.Entry.SmartPriorP50))
	fmt.Fprintf(w, "  prior KOL P50       %s\n", twoNum(prof.Entry.KOLPriorP50))
	fmt.Fprintf(w, "  cluster >=3         %s\n", twoPct(prof.Entry.Cluster3Plus))
	fmt.Fprintf(w, "\nPOSITION\n")
	fmt.Fprintf(w, "  episodes            %d  (confirmed %d · visible %d · censored %d · gap %d)\n",
		prof.Position.Episodes,
		prof.Position.Evidence.Confirmed, prof.Position.Evidence.Visible,
		prof.Position.Evidence.Censored, prof.Position.Evidence.DataGap)
	fmt.Fprintf(w, "  median adds         %s\n", twoNum(prof.Position.MedianAdds))
	fmt.Fprintf(w, "  median reduces      %s\n", twoNum(prof.Position.MedianReduces))
	fmt.Fprintf(w, "  median hold         %s\n", twoSecs(prof.Position.MedianHoldSecs))
	fmt.Fprintf(w, "\nEXIT\n")
	fmt.Fprintf(w, "  partial exits       %s\n", twoPct(prof.Exit.PartialExitRatio))
	fmt.Fprintf(w, "  first sell P50      %s\n", twoSecs(prof.Exit.FirstSellP50))
	fmt.Fprintf(w, "  full close P50      %s\n", twoSecs(prof.Exit.CloseP50))
	fmt.Fprintf(w, "  closed pnl          %s\n", twoPnl(prof.Exit.ClosedPnl, prof.Exit.ClosedWinRate))
	fmt.Fprintf(w, "  incomplete          %.0f%% episodes (data gap) pnl $%.0f\n",
		prof.Exit.IncompleteRatio*100, prof.Exit.IncompletePnl)
	if prof.Exit.CensoredPnl != 0 {
		fmt.Fprintf(w, "  censored pnl        $%.0f (origin unknown, complete data)\n", prof.Exit.CensoredPnl)
	}
	fmt.Fprintf(w, "\n  confirmed = independently proven zero balance (no source yet → n/a)\n")
	fmt.Fprintf(w, "  research  = visible + confirmed zero balance — research use only, never fact\n")
	return nil
}

// inCohort reports whether an entry belongs to the profile cohort: its
// EPISODE must have opened inside the analysis window. Adds are judged by
// their opening time (TradeTime - SinceInitialSecs) — an add to a position
// opened before the window is not part of this cohort, keeping the card's
// episodes/adds/exits all describing the same set of positions.
func inCohort(ce storage.ClassifiedEntry, sinceUnix int64) bool {
	opening := ce.TradeTime
	if !ce.Initial {
		opening = ce.TradeTime - ce.SinceInitialSecs
	}
	return opening >= sinceUnix
}

// two* helpers render the dual evidence columns: confirmed | inferred.
func twoCells(strict, research mechanism.MedianStat, fmtFn func(mechanism.MedianStat) string) string {
	s := "n/a"
	if strict.N > 0 {
		s = fmtFn(strict)
	}
	r := "n/a"
	if research.N > 0 {
		r = fmtFn(research)
	}
	return fmt.Sprintf("%-11s %-11s", s, r)
}

func twoPct(t mechanism.TwoStat) string  { return twoCells(t.Strict, t.Research, pctMed) }
func twoUsd(t mechanism.TwoStat) string  { return twoCells(t.Strict, t.Research, usdMed) }
func twoSecs(t mechanism.TwoStat) string { return twoCells(t.Strict, t.Research, secsMed) }
func twoNum(t mechanism.TwoStat) string  { return twoCells(t.Strict, t.Research, numMed) }

func twoPnl(pnl, win mechanism.TwoStat) string {
	s, r := "n/a", "n/a"
	if pnl.Strict.N > 0 {
		s = fmt.Sprintf("$%.0f (%.0f%% win, %d)", pnl.Strict.Value, win.Strict.Value*100, pnl.Strict.N)
	}
	if pnl.Research.N > 0 {
		r = fmt.Sprintf("$%.0f (%.0f%% win, %d)", pnl.Research.Value, win.Research.Value*100, pnl.Research.N)
	}
	return fmt.Sprintf("%-11s %-11s", s, r)
}

func usdMed(m mechanism.MedianStat) string {
	return fmt.Sprintf("$%.0f (%d)", m.Value, m.N)
}

func secsMed(m mechanism.MedianStat) string {
	return fmt.Sprintf("%.0fs (%d)", m.Value, m.N)
}

func numMed(m mechanism.MedianStat) string {
	return fmt.Sprintf("%.1f (%d)", m.Value, m.N)
}

func pctMed(m mechanism.MedianStat) string {
	return fmt.Sprintf("%+.1f%% (%d)", m.Value, m.N)
}

func usd(m mechanism.MedianStat) string {
	if m.N == 0 {
		return "n/a"
	}
	return fmt.Sprintf("$%.0f (%d)", m.Value, m.N)
}

func secs(m mechanism.MedianStat) string {
	if m.N == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.0fs (%d)", m.Value, m.N)
}

func num(m mechanism.MedianStat) string {
	if m.N == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f (%d)", m.Value, m.N)
}
