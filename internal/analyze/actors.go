package analyze

import (
	"fmt"
	"io"
	"math"
	"sort"
	"time"

	"github.com/auren23/followedge/internal/storage"
)

// Actor is the ranked summary of one wallet.
//
// Quality and Replication are deliberately separate axes:
//   - Quality: did this actor actually make money (realized PnL from the
//     GMGN feed, consistency, concentration, drawdown)?
//   - Replication: the coverage-aware census of what a follower at our
//     latency would have captured (follower markouts at a reference
//     horizon). No single 0-100 score: the facts are shown as-is
//     (coverage, observed EV, conservative EV, loss decomposition), so a
//     wallet with 10 great fills and 90 dead tokens can't masquerade as
//     highly replicable.
type Actor struct {
	Wallet     string
	WalletType string

	Trades   int
	Buys     int
	Sells    int
	TotalUSD float64

	RealizedPnL    float64
	ProfitableDays int
	ActiveDays     int
	Top1Share      float64 // top-1 token's share of realized PnL (0-1)
	MaxDrawdown    float64 // USD, from the cumulative daily PnL curve

	Quality float64

	// replication census at the reference horizon (follower, coverage-aware)
	Due, Filled, MarketLoss, MeasLoss, Unresolved int
	ObservedEV                                    float64
	ObservedValid                                 bool
	ConsEV                                        float64
	ConsValid                                     bool
}

// qualityScore combines v0.1 heuristics into 0-100. Formulas are deliberately
// transparent (no ML): linear profit ramp to $10k, profitable-day share,
// trade sample size, top-1 concentration penalty, and daily-curve drawdown.
func qualityScore(a *Actor) float64 {
	profit := clamp01(a.RealizedPnL/10000) * 100
	consistency := 0.0
	if a.ActiveDays > 0 {
		consistency = float64(a.ProfitableDays) / float64(a.ActiveDays) * 100
	}
	sample := math.Min(100, float64(a.Trades))
	concentration := 100.0
	if a.RealizedPnL > 0 {
		concentration = (1 - clamp01(a.Top1Share)) * 100
	}
	dd := (1 - clamp01(a.MaxDrawdown/1000)) * 100

	return 0.30*profit + 0.25*consistency + 0.15*sample +
		0.15*concentration + 0.15*dd
}

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }

// actorRows buckets storage.ActorGroups into per-wallet Actor summaries.
func actorRows(groups []storage.ActorGroup) map[string]*Actor {
	byWallet := map[string]*Actor{}
	for _, g := range groups {
		a := byWallet[g.Wallet]
		if a == nil {
			a = &Actor{Wallet: g.Wallet, WalletType: g.WalletType}
			byWallet[g.Wallet] = a
		}
		a.Trades += g.Trades
		a.Buys += g.Buys
		a.Sells += g.Sells
		a.TotalUSD += g.TotalUSD
		a.RealizedPnL += g.RealizedPnL
	}
	// per-day and per-token PnL from the same groups
	dayPnl := map[string]map[string]float64{}   // wallet → day → pnl
	tokenPnl := map[string]map[string]float64{} // wallet → token → pnl
	for _, g := range groups {
		if dayPnl[g.Wallet] == nil {
			dayPnl[g.Wallet] = map[string]float64{}
		}
		dayPnl[g.Wallet][g.Day] += g.RealizedPnL
		if tokenPnl[g.Wallet] == nil {
			tokenPnl[g.Wallet] = map[string]float64{}
		}
		tokenPnl[g.Wallet][g.Token] += g.RealizedPnL
	}
	for wallet, a := range byWallet {
		for _, p := range dayPnl[wallet] {
			a.ActiveDays++
			if p > 0 {
				a.ProfitableDays++
			}
		}
		// cumulative daily curve → max drawdown
		days := make([]string, 0, len(dayPnl[wallet]))
		for d := range dayPnl[wallet] {
			days = append(days, d)
		}
		sort.Strings(days)
		var cum, peak, maxDD float64
		for _, d := range days {
			cum += dayPnl[wallet][d]
			peak = math.Max(peak, cum)
			maxDD = math.Max(maxDD, peak-cum)
		}
		a.MaxDrawdown = maxDD
		// top-1 token concentration (only when profitable)
		var top float64
		for _, p := range tokenPnl[wallet] {
			if p > top {
				top = p
			}
		}
		if a.RealizedPnL > 0 {
			a.Top1Share = top / a.RealizedPnL
		}
		a.Quality = qualityScore(a)
	}
	return byWallet
}

// attachReplication fills the coverage-aware replication census facts. consEV
// is computed like LatencyEV: unpriced MARKET-outcome rows (market loss)
// assumed to lose noExitLoss%; measurement failures and unresolved rows are
// excluded — a 429 or GMGN no-data is not a -100% trade.
func attachReplication(a *Actor, r *storage.ReplicationRow, noExitLoss float64) {
	if r == nil {
		return
	}
	a.Due, a.Filled, a.MarketLoss, a.MeasLoss, a.Unresolved = r.Due, r.Filled, r.MarketLoss, r.MeasLoss, r.Unresolved
	a.ObservedEV, a.ObservedValid = r.ObservedEV, r.ObservedValid
	market := r.Filled + r.MarketLoss
	if market > 0 {
		a.ConsEV = (r.ObservedEV*float64(r.Filled) - float64(r.MarketLoss)*noExitLoss) / float64(market)
		a.ConsValid = true
	}
}

// ActorSortKey is the --sort axis for `actors rank`.
type ActorSortKey string

const (
	SortQuality       ActorSortKey = "quality"
	SortReplicability ActorSortKey = "replicability" // conservative EV desc
	SortPnl           ActorSortKey = "pnl"
	SortCopyEV        ActorSortKey = "copy-ev" // observed EV desc
)

// rankActors sorts by the requested axis, then applies the optional Pareto
// frontier filter on (Quality, conservative EV). Replication axes honor a
// minimum sample gate (--min-repl-due/--min-repl-filled): an actor with
// N=1 +300%% must not top the replicability sort nor enter the frontier.
func rankActors(list []*Actor, sortKey ActorSortKey, frontier bool, minReplDue, minReplFilled int) []*Actor {
	replEligible := func(a *Actor) bool {
		return a.Due >= minReplDue && a.Filled >= minReplFilled
	}
	switch sortKey {
	case SortReplicability:
		sort.SliceStable(list, func(i, j int) bool {
			ei, ej := replEligible(list[i]) && list[i].ConsValid, replEligible(list[j]) && list[j].ConsValid
			if ei != ej {
				return ei
			}
			if !ei {
				return false // both ineligible: keep stable order
			}
			return list[i].ConsEV > list[j].ConsEV
		})
	case SortPnl:
		sort.SliceStable(list, func(i, j int) bool { return list[i].RealizedPnL > list[j].RealizedPnL })
	case SortCopyEV:
		sort.SliceStable(list, func(i, j int) bool {
			ei := replEligible(list[i]) && list[i].ObservedValid
			ej := replEligible(list[j]) && list[j].ObservedValid
			if ei != ej {
				return ei
			}
			if !ei {
				return false
			}
			return list[i].ObservedEV > list[j].ObservedEV
		})
	default: // quality
		sort.SliceStable(list, func(i, j int) bool { return list[i].Quality > list[j].Quality })
	}

	if frontier {
		// Pareto frontier on (Quality, consEV): keep only actors not strictly
		// dominated — nobody else has BOTH more quality AND more conservative
		// replication EV. Sample gate first: actors without enough replication
		// data cannot claim a frontier slot. ponytail: O(n²), fine at hundreds
		// of actors.
		cons := func(a *Actor) float64 {
			if !a.ConsValid || !replEligible(a) {
				return -math.MaxFloat64
			}
			return a.ConsEV
		}
		out := list[:0]
		for i, a := range list {
			if !replEligible(a) || !a.ConsValid {
				continue // no replication evidence → no frontier slot
			}
			dominated := false
			for j, b := range list {
				if i == j {
					continue
				}
				if b.Quality >= a.Quality && cons(b) >= cons(a) &&
					(b.Quality > a.Quality || cons(b) > cons(a)) {
					dominated = true
					break
				}
			}
			if !dominated {
				out = append(out, a)
			}
		}
		list = out
	}
	return list
}

// Rank prints the actor leaderboard with the coverage-aware replication
// census: every due follower row is bucketed (filled / market loss /
// measurement loss / unresolved), so a flattering filled-only mean can't
// hide survivor bias. Sorting and the Pareto frontier are user-selectable.
func Rank(w io.Writer, s *storage.Store, since time.Time, horizon time.Duration,
	limit, minTrades int, noExitLoss float64, grace time.Duration, sortKey ActorSortKey,
	frontier bool, minReplDue, minReplFilled int) error {
	groups, err := s.ActorGroups(since)
	if err != nil {
		return err
	}
	actors := actorRows(groups)

	// coverage-aware replication census per wallet at the reference horizon,
	// SAME window as Quality (since) and buy-side only — the two axes of the
	// card must describe the same period and the same question.
	census, err := s.ReplicationCensus(since, horizon, grace, time.Now().UTC())
	if err != nil {
		return err
	}
	for _, r := range census {
		if a := actors[r.Wallet]; a != nil {
			attachReplication(a, &r, noExitLoss)
		}
	}

	list := make([]*Actor, 0, len(actors))
	for _, a := range actors {
		if a.Trades >= minTrades {
			list = append(list, a)
		}
	}
	list = rankActors(list, sortKey, frontier, minReplDue, minReplFilled)
	if limit > 0 && len(list) > limit {
		list = list[:limit]
	}

	// persist E1 evidence (GMGN-derived) for every ranked actor: the table
	// keeps period history even as windows move
	for _, a := range list {
		pnl := a.RealizedPnL
		if err := s.UpsertEvidence(a.Wallet, "realized_pnl", storage.EvidenceE1, "gmgn_feed",
			since, time.Now().UTC(), a.Trades, &pnl, nil); err != nil {
			fmt.Fprintf(w, "(evidence upsert failed for %s: %v)\n", a.Wallet, err)
		}
	}

	fmt.Fprintf(w, "ACTORS (since %s, markout horizon %v) — Quality vs Replication are separate axes; replication is coverage-aware\n",
		since.Format("2006-01-02"), horizon)
	if frontier {
		fmt.Fprintf(w, "Pareto frontier on (quality, consEV) — nobody strictly dominates another actor on both axes\n")
	}
	fmt.Fprintf(w, "%-46s %6s %6s %6s %10s %9s %6s %7s %7s %6s %7s %7s %9s %10s\n",
		"wallet", "trades", "buys", "sells", "real_pnl", "consist", "top1%", "dd",
		"quality", "due", "filled", "cov%", "obsEV", "consEV")
	for _, a := range list {
		top1 := math.Min(100, a.Top1Share*100) // lottery winners can exceed 100% of net
		obsEV := "-"
		if a.ObservedValid {
			obsEV = fmt.Sprintf("%+8.2f%%", a.ObservedEV)
		}
		consEV := "n/a"
		if a.ConsValid {
			consEV = fmt.Sprintf("%+9.2f%%", a.ConsEV)
		}
		fmt.Fprintf(w, "%-46s %6d %6d %6d %10.0f %8.0f%% %5.0f%% %6.0f %7.1f %6d %7d %6.1f%% %9s %10s\n",
			a.Wallet, a.Trades, a.Buys, a.Sells, a.RealizedPnL,
			pct(a.ProfitableDays, a.ActiveDays), top1, a.MaxDrawdown,
			a.Quality, a.Due, a.Filled, pct(a.Filled, a.Due), obsEV, consEV)
	}
	if len(list) == 0 {
		fmt.Fprintln(w, "(no actors with enough data yet — keep collecting)")
	}
	return nil
}

func pct(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d) * 100
}

// Inspect prints one actor's full research card: PnL facts + alpha decay +
// the coverage-aware replication census at each configured horizon.
func Inspect(w io.Writer, s *storage.Store, wallet string, since time.Time,
	horizons []time.Duration, noExitLoss float64, grace time.Duration) error {
	groups, err := s.ActorGroups(since)
	if err != nil {
		return err
	}
	filtered := groups[:0]
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
	if a == nil {
		return fmt.Errorf("no data for %s", wallet)
	}

	fmt.Fprintf(w, "ACTOR %s (%s)\n", wallet, a.WalletType)
	fmt.Fprintf(w, "  trades:      %d (buy %d / sell %d)\n", a.Trades, a.Buys, a.Sells)
	fmt.Fprintf(w, "  volume:      $%.0f\n", a.TotalUSD)
	fmt.Fprintf(w, "  realized pnl: $%.0f  (from sell legs: amount_usd - buy_cost_usd)\n", a.RealizedPnL)
	fmt.Fprintf(w, "  consistency: %d/%d profitable days (%.0f%%)\n",
		a.ProfitableDays, a.ActiveDays, pct(a.ProfitableDays, a.ActiveDays))
	fmt.Fprintf(w, "  top1 share:  %.0f%%   max drawdown: $%.0f\n", a.Top1Share*100, a.MaxDrawdown)
	fmt.Fprintf(w, "  quality:     %.1f\n", a.Quality)

	// evidence card: best level + all rows
	rows, err := s.EvidenceFor(wallet)
	if err == nil && len(rows) > 0 {
		best := rows[0]
		for _, r := range rows[1:] {
			if storage.EvidenceRank[r.Level] > storage.EvidenceRank[best.Level] {
				best = r
			}
		}
		fmt.Fprintf(w, "  best evidence: %s (%s)\n", best.Level, best.Source)
		for _, r := range rows {
			pnlStr := "-"
			if r.RealizedPnL != nil {
				pnlStr = fmt.Sprintf("$%.0f", *r.RealizedPnL)
			}
			fmt.Fprintf(w, "    %s %s %-8s %s → %s  n=%d pnl=%s\n", r.Level, r.Source, "period",
				time.Unix(r.PeriodStart, 0).Format("2006-01-02"),
				time.Unix(r.PeriodEnd, 0).Format("2006-01-02"), r.TradeCount, pnlStr)
		}
	} else if err != nil {
		fmt.Fprintf(w, "  (evidence query failed: %v)\n", err)
	}

	fmt.Fprintf(w, "\nALPHA DECAY (follower: entry at ReceivedAt, mean return of buys)\n")
	fmt.Fprintf(w, "%-10s %8s %8s %8s\n", "horizon", "fills", "avg", "WR")
	for _, h := range horizons {
		rets := buysAt(s, wallet, h)
		if len(rets) == 0 {
			continue
		}
		wins := 0
		for _, r := range rets {
			if r > 0 {
				wins++
			}
		}
		fmt.Fprintf(w, "%-10s %8d %+7.2f%% %7.1f%%\n", h.String(), len(rets), mean(rets),
			float64(wins)/float64(len(rets))*100)
	}

	// coverage-aware replication census per horizon — the survivor-bias guard,
	// same window as the card and buy-side only
	now := time.Now().UTC()
	for _, h := range horizons {
		census, err := s.ReplicationCensus(since, h, grace, now)
		if err != nil {
			return err
		}
		var r *storage.ReplicationRow
		for i := range census {
			if census[i].Wallet == wallet {
				r = &census[i]
				break
			}
		}
		if r == nil || r.Due == 0 {
			continue
		}
		consStr := "n/a"
		market := r.Filled + r.MarketLoss
		if market > 0 {
			cons := (r.ObservedEV*float64(r.Filled) - float64(r.MarketLoss)*noExitLoss) / float64(market)
			consStr = fmt.Sprintf("%+9.2f%%", cons)
		}
		obsStr := "-"
		if r.ObservedValid {
			obsStr = fmt.Sprintf("%+9.2f%%", r.ObservedEV)
		}
		fmt.Fprintf(w, "\nREPLICATION @%v (follower — coverage-aware census)\n", h)
		fmt.Fprintf(w, "  due:              %d\n", r.Due)
		fmt.Fprintf(w, "  filled:           %d    coverage %.1f%%\n", r.Filled, pct(r.Filled, r.Due))
		fmt.Fprintf(w, "  observed EV:      %s\n", obsStr)
		fmt.Fprintf(w, "  conservative EV:  %s  (unpriced market-outcome rows assumed -%.0f%%)\n", consStr, noExitLoss)
		fmt.Fprintf(w, "  market loss:      %d  (no_candle/token_inactive/stale_outcome)\n", r.MarketLoss)
		fmt.Fprintf(w, "  measurement loss: %d  (api/rate/lookback/parse/no_kline)\n", r.MeasLoss)
		fmt.Fprintf(w, "  unresolved:       %d  (worker lag)\n", r.Unresolved)
	}

	// the EV cliff in one line: does chasing kill this actor's edge?
	// OBSERVED-ONLY: filled rows; coverage/market loss are in the census above.
	const refHorizon = 5 * time.Minute
	low := chaseConditionedEV(s, wallet, refHorizon, func(c float64) bool { return c <= 5 })
	high := chaseConditionedEV(s, wallet, refHorizon, func(c float64) bool { return c > 10 })
	if low.n > 0 || high.n > 0 {
		fmt.Fprintf(w, "\nCHASE-CONDITIONED EV @ %v (follower, OBSERVED-ONLY — filled rows; coverage in REPLICATION census above)\n", refHorizon)
		fmt.Fprintf(w, "%-14s %8s %10s\n", "condition", "N", "avg")
		if low.n > 0 {
			fmt.Fprintf(w, "%-14s %8d %+9.2f%%\n", "chase <= 5%", low.n, low.mean)
		}
		if high.n > 0 {
			fmt.Fprintf(w, "%-14s %8d %+9.2f%%\n", "chase > 10%", high.n, high.mean)
		}
	}
	return nil
}

// buysAt collects one wallet's filled FOLLOWER buy markouts at a horizon.
func buysAt(s *storage.Store, wallet string, horizon time.Duration) []float64 {
	rows, err := s.MarkoutsAt(storage.MarkoutFollower, horizon)
	if err != nil {
		return nil
	}
	var out []float64
	for _, m := range rows {
		if m.Wallet == wallet && m.Side == "buy" && m.ReturnPct.Valid {
			out = append(out, m.ReturnPct.Float64)
		}
	}
	return out
}

// chaseConditionedEV is the follower EV of a wallet's buys whose entry chase
// satisfies the condition — "when I enter cheaply vs late, what happens".
type condEV struct {
	n    int
	mean float64
}

func chaseConditionedEV(s *storage.Store, wallet string, horizon time.Duration, cond func(float64) bool) condEV {
	rows, err := s.MarkoutsAt(storage.MarkoutFollower, horizon)
	if err != nil {
		return condEV{}
	}
	var rets []float64
	for _, m := range rows {
		if m.Wallet != wallet || m.Side != "buy" || !m.ReturnPct.Valid {
			continue
		}
		if cond(m.ChasePct) {
			rets = append(rets, m.ReturnPct.Float64)
		}
	}
	return condEV{n: len(rets), mean: mean(rets)}
}
