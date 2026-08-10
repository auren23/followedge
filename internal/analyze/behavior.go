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
	episodes, err := s.ReconstructEpisodesFor(wallet, since)
	if err != nil {
		return err
	}
	classified, err := s.ClassifiedEntries(wallet)
	if err != nil {
		return err
	}
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
	fmt.Fprintf(w, "\nENTRY\n")
	fmt.Fprintf(w, "  initial buys    %d\n", prof.Entry.InitialCount)
	fmt.Fprintf(w, "  add buys        %d\n", prof.Entry.AddCount)
	fmt.Fprintf(w, "  reentry rate    %.0f%%\n", prof.Entry.ReentryRate*100)
	fmt.Fprintf(w, "  initial buy     %s\n", usd(prof.Entry.MedianInitialBuy))
	fmt.Fprintf(w, "  add buy         %s (%.0f%% of episodes add)\n",
		usd(prof.Entry.MedianAddBuy), prof.Entry.AddEpisodeRate*100)
	fmt.Fprintf(w, "  total capital   %s\n", usd(prof.Entry.MedianCapitalIn))
	fmt.Fprintf(w, "  median age      %s\n", secs(prof.Entry.MedianAge))
	fmt.Fprintf(w, "  median chase    %s (initial only)\n", pctMed(prof.Entry.MedianChase))
	fmt.Fprintf(w, "  median add chase %s\n", pctMed(prof.Entry.MedianAddChase))
	fmt.Fprintf(w, "  add since open  %s\n", secs(prof.Entry.MedianSinceInitialSecs))
	fmt.Fprintf(w, "  prior smart P50 %s\n", num(prof.Entry.SmartPriorP50))
	fmt.Fprintf(w, "  prior KOL P50   %s\n", num(prof.Entry.KOLPriorP50))
	fmt.Fprintf(w, "  cluster >=3     %.0f%% (%d valid prior windows)\n",
		prof.Entry.Cluster3Plus*100, prof.Entry.PriorFlowN)
	fmt.Fprintf(w, "\nPOSITION\n")
	fmt.Fprintf(w, "  episodes        %d\n", prof.Position.Episodes)
	fmt.Fprintf(w, "  median adds     %s\n", num(prof.Position.MedianAdds))
	fmt.Fprintf(w, "  median reduces  %s\n", num(prof.Position.MedianReduces))
	fmt.Fprintf(w, "  median hold     %s\n", secs(prof.Position.MedianHoldSecs))
	fmt.Fprintf(w, "\nEXIT\n")
	if prof.Exit.ObservableN > 0 {
		fmt.Fprintf(w, "  partial exits   %.0f%% (%d of %d observable)\n",
			prof.Exit.PartialExitRatio*100, prof.Exit.PartialExitN, prof.Exit.ObservableN)
	} else {
		fmt.Fprintf(w, "  partial exits   n/a (no observable exits)\n")
	}
	fmt.Fprintf(w, "  first sell P50  %s\n", secs(prof.Exit.FirstSellP50))
	fmt.Fprintf(w, "  full close P50  %s\n", secs(prof.Exit.CloseP50))
	fmt.Fprintf(w, "  closed pnl      $%.0f  (%.0f%% win rate, %d closed)\n",
		prof.Exit.ClosedPnl, prof.Exit.ClosedWinRate*100, prof.Exit.CloseP50.N)
	fmt.Fprintf(w, "  incomplete      %.0f%%  (data gap: opening buys unseen) pnl $%.0f\n",
		prof.Exit.IncompleteRatio*100, prof.Exit.IncompletePnl)
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

func pctMed(m mechanism.MedianStat) string {
	if m.N == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%+.1f%% (%d)", m.Value, m.N)
}
