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
// Point-in-time: entry-context features (prior smart/KOL buyers, chase) only
// use data knowable at each entry's trade_time.
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

	// ---- BEHAVIOR (episodes + entry context, same window) ----
	episodes, err := s.EpisodesFor(wallet)
	if err != nil {
		return err
	}
	var inWindow []storage.Episode
	for _, e := range episodes {
		if e.OpenedAt >= since.Unix() {
			inWindow = append(inWindow, e)
		}
	}
	entries, err := s.EntryRows(wallet, since)
	if err != nil {
		return err
	}
	firstSells, err := s.FirstSellDelays(wallet, since)
	if err != nil {
		return err
	}
	chases, err := walletChases(s, wallet, horizon, since)
	if err != nil {
		return err
	}
	var smartPrior, kolPrior []float64
	for _, e := range entries {
		cs, ok, err := s.ClusterStateAt(e.Token, e.TradeTime, clusterWindow)
		if err != nil {
			return err
		}
		if ok {
			smartPrior = append(smartPrior, float64(cs.SmartBuyWallets))
			kolPrior = append(kolPrior, float64(cs.KOLBuyWallets))
		}
	}

	prof := mechanism.BuildProfile(wallet, inWindow, entries, firstSells, chases, smartPrior, kolPrior)

	fmt.Fprintf(w, "\nBEHAVIOR (since %s)\n", since.Format("2006-01-02"))
	fmt.Fprintf(w, "\nENTRY\n")
	fmt.Fprintf(w, "  entries         %d\n", prof.Entry.Count)
	fmt.Fprintf(w, "  token reuse     %.0f%%\n", prof.Entry.TokenReuse*100)
	fmt.Fprintf(w, "  median size     $%.0f\n", prof.Entry.MedianSize)
	fmt.Fprintf(w, "  median age      %.0fs\n", prof.Entry.MedianAge)
	fmt.Fprintf(w, "  median chase    %+.1f%%\n", prof.Entry.MedianChase)
	fmt.Fprintf(w, "  prior smart P50 %.1f\n", prof.Entry.SmartPriorP50)
	fmt.Fprintf(w, "  prior KOL P50   %.1f\n", prof.Entry.KOLPriorP50)
	fmt.Fprintf(w, "  cluster >=3     %.0f%%\n", prof.Entry.Cluster3Plus*100)
	fmt.Fprintf(w, "\nPOSITION\n")
	fmt.Fprintf(w, "  episodes        %d\n", prof.Position.Episodes)
	fmt.Fprintf(w, "  median adds     %.0f\n", prof.Position.MedianAdds)
	fmt.Fprintf(w, "  median reduces  %.0f\n", prof.Position.MedianReduces)
	fmt.Fprintf(w, "  median capital  $%.0f in / $%.0f out\n",
		prof.Position.MedianCapitalIn, prof.Position.MedianCapitalOut)
	fmt.Fprintf(w, "  median hold     %.0fs\n", prof.Position.MedianHoldSecs)
	fmt.Fprintf(w, "\nEXIT\n")
	fmt.Fprintf(w, "  partial exit    %.0f%%\n", prof.Exit.PartialRatio*100)
	fmt.Fprintf(w, "  full close      %.0f%%\n", prof.Exit.FullRatio*100)
	fmt.Fprintf(w, "  first sell P50  %.0fs\n", prof.Exit.MedianFirstSellSecs)
	fmt.Fprintf(w, "  full close P50  %.0fs\n", prof.Exit.MedianCloseSecs)
	fmt.Fprintf(w, "  pnl             $%.0f  (%.0f%% profitable closed)\n",
		prof.Exit.TotalPnl, prof.Exit.ProfitableRatio*100)
	return nil
}

// walletChases collects the follower entry chase % of a wallet's buys in the
// window (PIT: chase is known at entry).
func walletChases(s *storage.Store, wallet string, horizon time.Duration, since time.Time) ([]float64, error) {
	rows, err := s.MarkoutsAt(storage.MarkoutFollower, horizon)
	if err != nil {
		return nil, err
	}
	var out []float64
	for _, m := range rows {
		if m.Wallet == wallet && m.Side == "buy" && m.TradeTime >= since.Unix() {
			out = append(out, m.ChasePct)
		}
	}
	return out, nil
}
