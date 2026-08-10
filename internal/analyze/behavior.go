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
// from trade_events — never from the possibly-stale materialized table.
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

	// ---- BEHAVIOR (on-demand reconstruction, same window) ----
	episodes, err := s.ReconstructEpisodesFor(wallet, since)
	if err != nil {
		return err
	}
	entries, err := s.EntryRows(wallet, since)
	if err != nil {
		return err
	}
	obs, err := s.EntryObservations(wallet, since, horizon)
	if err != nil {
		return err
	}
	var chases []float64
	for _, o := range obs {
		chases = append(chases, o.ChasePct)
	}
	var smartPrior, kolPrior []float64
	for _, e := range entries {
		smart, kol, err := s.PriorFlowAt(e.Token, e.TradeTime, clusterWindow)
		if err != nil {
			return err
		}
		smartPrior = append(smartPrior, float64(smart))
		kolPrior = append(kolPrior, float64(kol))
	}

	prof := mechanism.BuildProfile(wallet, episodes, entries, chases, smartPrior, kolPrior)

	fmt.Fprintf(w, "\nBEHAVIOR (since %s)\n", since.Format("2006-01-02"))
	fmt.Fprintf(w, "\nENTRY\n")
	fmt.Fprintf(w, "  entries         %d\n", prof.Entry.Count)
	fmt.Fprintf(w, "  reentry rate    %.0f%%\n", prof.Entry.ReentryRate*100)
	fmt.Fprintf(w, "  initial buy     %s\n", usd(prof.Entry.MedianInitialBuy))
	fmt.Fprintf(w, "  add buy         %s\n", usd(prof.Entry.MedianAddBuy))
	fmt.Fprintf(w, "  total capital   %s\n", usd(prof.Entry.MedianCapitalIn))
	fmt.Fprintf(w, "  median age      %s\n", secs(prof.Entry.MedianAge))
	fmt.Fprintf(w, "  median chase    %s\n", pctMed(prof.Entry.MedianChase))
	fmt.Fprintf(w, "  prior smart P50 %s\n", num(prof.Entry.SmartPriorP50))
	fmt.Fprintf(w, "  prior KOL P50   %s\n", num(prof.Entry.KOLPriorP50))
	fmt.Fprintf(w, "  cluster >=3     %.0f%% (%d observed)\n",
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
