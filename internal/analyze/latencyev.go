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

var chaseCols = []string{"<0%", "0-5%", "5-10%", "10-20%", "20%+"}

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
// edge survive a 140s REST feed? Follower EV bucketed by source age (and,
// with byChase, by entry chase in a two-dimensional matrix), per wallet type.
func LatencyEV(w io.Writer, s *storage.Store, horizon time.Duration, side string, byChase bool) error {
	rows, err := s.MarkoutsAt(storage.MarkoutFollower, horizon)
	if err != nil {
		return err
	}
	// wallet_type → age bucket → [chase col] → returns
	type cell struct {
		n    int
		rets []float64
	}
	byType := map[string]map[string]map[string]*cell{}
	for _, m := range rows {
		if !m.ReturnPct.Valid || (side != "" && m.Side != side) {
			continue
		}
		age := float64(m.ReceivedAt - m.TradeTime)
		byAge := byType[m.WalletType]
		if byAge == nil {
			byAge = map[string]map[string]*cell{}
			byType[m.WalletType] = byAge
		}
		chaseMap := byAge[ageBucket(age)]
		if chaseMap == nil {
			chaseMap = map[string]*cell{}
			byAge[ageBucket(age)] = chaseMap
		}
		key := ""
		if byChase {
			key = chaseCol(m.ChasePct)
		}
		c := chaseMap[key]
		if c == nil {
			c = &cell{}
			chaseMap[key] = c
		}
		c.n++
		c.rets = append(c.rets, m.ReturnPct.Float64)
	}

	types := make([]string, 0, len(byType))
	for t := range byType {
		types = append(types, t)
	}
	sort.Strings(types)

	for _, t := range types {
		fmt.Fprintf(w, "\nSOURCE AGE × FOLLOWER EV @ %v — %s (entry = last closed candle at reception)\n",
			horizon, t)
		byAge := byType[t]
		if byChase {
			fmt.Fprintf(w, "%-10s %10s %10s %10s %10s %10s %10s\n", "age", "N", "<0%", "0-5%", "5-10%", "10-20%", "20%+")
			for _, b := range ageOrder {
				row := byAge[b]
				if row == nil {
					continue
				}
				var totalN int
				for _, c := range row {
					totalN += c.n
				}
				fmt.Fprintf(w, "%-10s %10d", b, totalN)
				for _, col := range chaseCols {
					c := row[col]
					if c == nil || c.n == 0 {
						fmt.Fprintf(w, " %10s", "-")
						continue
					}
					fmt.Fprintf(w, " %+9.2f%%", mean(c.rets))
				}
				fmt.Fprintln(w)
			}
		} else {
			fmt.Fprintf(w, "%-10s %8s %8s %10s %10s\n", "age", "N", "WR", "avg", "median")
			for _, b := range ageOrder {
				row := byAge[b]
				if row == nil {
					continue
				}
				var rets []float64
				for _, c := range row {
					rets = append(rets, c.rets...)
				}
				wins := 0
				for _, r := range rets {
					if r > 0 {
						wins++
					}
				}
				fmt.Fprintf(w, "%-10s %8d %7.1f%% %+9.2f%% %+9.2f%%\n",
					b, len(rets), float64(wins)/float64(len(rets))*100, mean(rets), median(rets))
			}
		}
	}
	if len(types) == 0 {
		fmt.Fprintln(w, "(no filled follower markouts yet — keep collecting)")
	}
	return nil
}
