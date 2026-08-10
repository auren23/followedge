package analyze

import (
	"fmt"
	"io"
	"time"

	"github.com/auren23/followedge/internal/storage"
)

// Chase prints the core research table: chase bucket (how much the price had
// already moved by the time a follower could enter — entry/leader − 1) vs
// the follower's forward return at that horizon. The two variables are
// deliberately distinct: bucketing by chase and averaging the follower
// return answers "how late is too late?" (the EV cliff).
func Chase(w io.Writer, s *storage.Store, horizon time.Duration, side string) error {
	rows, err := s.MarkoutsAt(storage.MarkoutFollower, horizon)
	if err != nil {
		return err
	}
	buckets := map[string][]float64{}
	for _, m := range rows {
		if !m.ReturnPct.Valid {
			continue
		}
		if side != "" && m.Side != side {
			continue
		}
		b := Bucket(m.ChasePct)
		buckets[b] = append(buckets[b], m.ReturnPct.Float64)
	}
	order := []string{"<0%", "0-2%", "2-5%", "5-10%", "10-20%", "20%+"}
	fmt.Fprintf(w, "CHASE @ %v — follower EV vs entry chase (entry = price at ReceivedAt)\n", horizon)
	fmt.Fprintf(w, "%-8s %8s %8s %10s %10s\n", "chase", "N", "WR", "avg", "median")
	totalN := 0
	for _, b := range order {
		a := buckets[b]
		if len(a) == 0 {
			continue
		}
		wins := 0
		for _, v := range a {
			if v > 0 {
				wins++
			}
		}
		totalN += len(a)
		fmt.Fprintf(w, "%-8s %8d %7.1f%% %+9.2f%% %+9.2f%%\n",
			b, len(a), float64(wins)/float64(len(a))*100, mean(a), median(a))
	}
	fmt.Fprintf(w, "\n%d filled follower markouts at %v\n", totalN, horizon)
	return nil
}

// Wallets was superseded by `actors rank` (same data, plus realized PnL and
// quality/replicability scores). Removed to avoid two tables saying the same
// thing.
