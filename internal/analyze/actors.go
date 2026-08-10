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
// The two scores are deliberately separate:
//   - Quality: did this actor actually make money (realized PnL from the
//     GMGN feed, consistency, concentration, drawdown)?
//   - Replicability: could a follower at our latency capture it (mean
//     markout return of buys at a reference horizon, sample-adjusted)?
//
// A wallet can be rich but uncopyable, or modest but perfectly copyable —
// the project only cares about the latter.
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

	Quality       float64
	Replicability float64
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

// replicabilityScore: mean buy-markout return at the reference horizon,
// scaled (+10% = 100), sample-adjusted (20 fills = full), zero below break-even.
func replicabilityScore(evMean float64, fills int) float64 {
	if evMean <= 0 {
		return 0
	}
	sampleFactor := math.Min(1, float64(fills)/20)
	return clamp01(evMean/10) * 100 * sampleFactor
}

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }

// actorRows buckets storage.ActorGroups into per-wallet Actor summaries.
func actorRows(groups []storage.ActorGroup, buys map[string][]float64) map[string]*Actor {
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
		a.Replicability = replicabilityScore(mean(buys[wallet]), len(buys[wallet]))
	}
	return byWallet
}

// Rank prints the actor leaderboard: profitable + consistent + copyable.
func Rank(w io.Writer, s *storage.Store, since time.Time, horizon time.Duration, limit int, minTrades int) error {
	groups, err := s.ActorGroups(since)
	if err != nil {
		return err
	}
	// buy markout returns per wallet at the reference horizon
	markouts, err := s.MarkoutsAt(horizon)
	if err != nil {
		return err
	}
	buys := map[string][]float64{}
	for _, m := range markouts {
		if m.Side != "buy" || !m.ReturnPct.Valid {
			continue
		}
		buys[m.Wallet] = append(buys[m.Wallet], m.ReturnPct.Float64)
	}

	actors := actorRows(groups, buys)
	list := make([]*Actor, 0, len(actors))
	for _, a := range actors {
		if a.Trades >= minTrades {
			list = append(list, a)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Quality > list[j].Quality })
	if limit > 0 && len(list) > limit {
		list = list[:limit]
	}

	fmt.Fprintf(w, "ACTORS (since %s, markout horizon %v) — Quality vs Replicability are separate axes\n",
		since.Format("2006-01-02"), horizon)
	fmt.Fprintf(w, "%-46s %6s %6s %6s %10s %10s %6s %6s %8s %8s\n",
		"wallet", "trades", "buys", "sells", "real_pnl", "consist", "top1%", "dd", "quality", "repl")
	for _, a := range list {
		top1 := math.Min(100, a.Top1Share*100) // lottery winners can exceed 100% of net
		fmt.Fprintf(w, "%-46s %6d %6d %6d %10.0f %9.0f%% %5.0f%% %6.0f %7.1f %7.1f\n",
			a.Wallet, a.Trades, a.Buys, a.Sells, a.RealizedPnL,
			pct(a.ProfitableDays, a.ActiveDays), top1, a.MaxDrawdown,
			a.Quality, a.Replicability)
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

// Inspect prints one actor's full research card: PnL facts + alpha decay.
func Inspect(w io.Writer, s *storage.Store, wallet string, since time.Time, horizons []time.Duration) error {
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
	actors := actorRows(filtered, nil)
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
	fmt.Fprintf(w, "  quality:     %.1f\n\n", a.Quality)

	fmt.Fprintf(w, "ALPHA DECAY (mean markout return of buys, sample = fills)\n")
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
	return nil
}

// buysAt collects one wallet's filled buy markouts at a horizon.
func buysAt(s *storage.Store, wallet string, horizon time.Duration) []float64 {
	rows, err := s.MarkoutsAt(horizon)
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
