package analyze

import (
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/auren23/followedge/internal/storage"
)

// ageBucket groups a source age (seconds) into the research buckets.
func ageBucket(secs float64) string {
	switch {
	case secs <= 30:
		return "0-30s"
	case secs <= 60:
		return "30-60s"
	case secs <= 120:
		return "60-120s"
	case secs <= 180:
		return "120-180s"
	case secs <= 300:
		return "180-300s"
	default:
		return "300s+"
	}
}

var ageOrder = []string{"0-30s", "30-60s", "60-120s", "120-180s", "180-300s", "300s+"}

var chaseCols = []string{"<0%", "0-5%", "5-10%", "10-20%", "20%+", "n/a"}

// n/a marks rows whose chase is not computable (entry price unknown) — they
// must stay visible in the by-chase matrix, not vanish from its totals.

// chaseCol maps a chase percentage to the matrix column.
func chaseCol(pct float64) string {
	switch {
	case pct < 0:
		return "<0%"
	case pct < 5:
		return "0-5%"
	case pct < 10:
		return "5-10%"
	case pct < 20:
		return "10-20%"
	default:
		return "20%+"
	}
}

// LatencyEV answers the project's central open question: does any copyable
// edge survive a 140s REST feed? Census-driven: EVERY due row is bucketed
// (filled or not), so coverage and a conservative EV (unpriced samples
// assumed to lose noExitLoss%) are reported alongside the observed EV.
// This is the selection-bias guard — excluding dead tokens silently would
// LatencyEV answers the project's central open question: does any copyable
// edge survive a 140s REST feed? Census-driven: EVERY due row is bucketed
// (filled or not), so coverage and a conservative EV are reported alongside
// the observed EV. This is the selection-bias guard — excluding dead tokens
// silently would flatter the edge.
//
// Statuses split into two dimensions: MARKET outcome (filled, no_candle,
// token_inactive — and due-but-unclassified rows, conservatively) enters the
// cons-EV denominator; MEASUREMENT failure (api_error, rate_limited,
// lookback_miss, parse_error) is coverage loss, not trade loss, and is
// reported in the meas column instead. A 429 is not a -100% trade.
func LatencyEV(w io.Writer, s *storage.Store, horizon time.Duration, side string,
	byChase bool, noExitLoss float64, grace time.Duration) error {
	rows, err := s.MarkoutCensus(storage.MarkoutFollower, horizon, grace, time.Now().UTC())
	if err != nil {
		return err
	}

	type cell struct {
		due        int // all due rows (denominator of coverage)
		filled     int // priced with a computable return
		market     int // market-outcome rows (filled + no_candle + token_inactive + stale_outcome)
		meas       int // measurement failures (excluded from cons EV)
		unresolved int // due but not yet classified by the worker (pending)
		rets       []float64
	}

	// ---- bucket every due row (census) by wallet type × age × [chase] ----
	type key struct{ typ, age, col string }
	cells := map[key]*cell{}
	types := []string{}
	for _, r := range rows {
		if side != "" && r.Side != side {
			continue // --side is a real filter: the whole bucket is side-only
		}
		k := key{r.WalletType, ageBucket(float64(r.ReceivedAt - r.TradeTime)), ""}
		if byChase {
			if r.BasePrice > 0 && r.LeaderPrice > 0 {
				k.col = chaseCol((r.BasePrice/r.LeaderPrice - 1) * 100)
			} else {
				k.col = "n/a" // entry unknown: chase not computable, keep visible
			}
		}
		cl := cells[k]
		if cl == nil {
			cl = &cell{}
			cells[k] = cl
			if !containsStr(types, r.WalletType) {
				types = append(types, r.WalletType)
			}
		}
		cl.due++
		switch r.Status {
		case storage.MarkoutStatusFilled:
			cl.market++
			if r.ObservedPrice != nil && r.BasePrice > 0 {
				cl.filled++
				cl.rets = append(cl.rets, (*r.ObservedPrice/r.BasePrice-1)*100)
			}
		case storage.MarkoutStatusNoCandle, storage.MarkoutStatusTokenInactive, storage.MarkoutStatusStaleOutcome:
			cl.market++ // dead / stale tokens: conservative loss
		case storage.MarkoutStatusPending:
			// due but the worker has not classified it yet (tick lag,
			// maxTokensPerTick backlog). That is sampling throughput, not a
			// market outcome — keep it out of the cons-EV denominator.
			cl.unresolved++
		default: // api_error, rate_limited, lookback_miss, price_parse_error
			cl.meas++ // measurement loss — NOT a trade outcome
		}
	}
	sort.Strings(types)
	if len(types) == 0 {
		fmt.Fprintln(w, "(no follower markout rows yet — keep collecting)")
		return nil
	}

	for _, t := range types {
		fmt.Fprintf(w, "\nSOURCE AGE × FOLLOWER EV @ %v — %s (entry = last closed candle at reception)\n",
			horizon, t)
		if byChase {
			fmt.Fprintf(w, "%-10s %10s %10s %10s %10s %10s %10s %10s %10s\n", "age", "due", "fill", "<0%", "0-5%", "5-10%", "10-20%", "20%+", "n/a")
			fmt.Fprintf(w, "%-10s %10s %10s %10s %10s %10s %10s %10s %10s\n", "", "", "cov%", "avg", "avg", "avg", "avg", "avg", "avg")
			for _, b := range ageOrder {
				var totalN int
				for _, col := range chaseCols {
					if c := cells[key{t, b, col}]; c != nil {
						totalN += c.due
					}
				}
				if totalN == 0 {
					continue
				}
				fmt.Fprintf(w, "%-10s %10d", b, totalN)
				filled := 0
				for _, col := range chaseCols {
					c := cells[key{t, b, col}]
					if c == nil || len(c.rets) == 0 {
						fmt.Fprintf(w, " %10s", "-")
						continue
					}
					filled += c.filled
					fmt.Fprintf(w, " %+9.2f%%", mean(c.rets))
				}
				fmt.Fprintf(w, " %9.1f%%", pctOf(filled, totalN))
				fmt.Fprintln(w)
			}
			fmt.Fprintln(w, "cells: observed EV (avg return of filled), last column = overall coverage")
			continue
		}

		fmt.Fprintf(w, "%-10s %8s %8s %9s %10s %10s %12s %8s %10s\n",
			"age", "due", "fill", "cover", "obsEV", "consEV", "market_loss", "meas", "unresolved")
		for _, b := range ageOrder {
			c := cells[key{t, b, ""}]
			if c == nil || c.due == 0 {
				continue
			}
			// conservative EV: every unpriced MARKET-outcome due row assumed
			// to lose noExitLoss%; measurement failures AND pending rows are
			// excluded — a 429 or an unprocessed row is not a -100% trade.
			consStr := "n/a"
			if c.market > 0 {
				loss := float64(c.market-c.filled) * -noExitLoss
				cons := (sum(c.rets) + loss) / float64(c.market)
				consStr = fmt.Sprintf("%+9.2f%%", cons)
			}
			fmt.Fprintf(w, "%-10s %8d %8d %8.1f%% %+9.2f%% %10s %12d %8d %10d\n",
				b, c.due, c.filled, pctOf(c.filled, c.due),
				mean(c.rets), consStr, c.market-c.filled, c.meas, c.unresolved)
		}
	}
	fmt.Fprintf(w, "\ncons EV: unpriced MARKET-outcome rows (no_candle/token_inactive/stale_outcome) assumed to lose %.0f%%;\n", noExitLoss)
	fmt.Fprintf(w, "measurement failures (api/rate/lookback/parse) and unresolved (pending, worker lag) excluded — coverage loss, not trade loss.\n")
	return nil
}

func containsStr(a []string, s string) bool {
	for _, v := range a {
		if v == s {
			return true
		}
	}
	return false
}

func sum(a []float64) float64 {
	var s float64
	for _, v := range a {
		s += v
	}
	return s
}
