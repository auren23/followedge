package analyze

import (
	"fmt"
	"io"
	"time"

	"github.com/auren23/followedge/internal/storage"
)

// Coverage prints the markout status census: how many due rows were filled
// vs lost to each failure reason. This is the selection-bias guard — EV
// tables are only interpretable against it.
func Coverage(w io.Writer, s *storage.Store, kind string, horizons []time.Duration, grace time.Duration) error {
	now := time.Now().UTC()
	for _, h := range horizons {
		counts, pending, err := s.MarkoutStatusCounts(kind, h, grace, now)
		if err != nil {
			return err
		}
		var due int
		for _, n := range counts {
			due += n
		}
		fmt.Fprintf(w, "MARKOUT COVERAGE @%v (%s)\n", h, kind)
		if due == 0 && pending == 0 {
			fmt.Fprintf(w, "  (no rows yet)\n\n")
			continue
		}
		fmt.Fprintf(w, "  %-16s %10s\n", "status", "count")
		order := []string{
			storage.MarkoutStatusFilled,
			storage.MarkoutStatusNoCandle,
			storage.MarkoutStatusTokenInactive,
			storage.MarkoutStatusAPIError,
			storage.MarkoutStatusRateLimited,
			storage.MarkoutStatusLookbackMiss,
			storage.MarkoutStatusParseError,
			storage.MarkoutStatusPending,
		}
		for _, st := range order {
			n := counts[st]
			if st == storage.MarkoutStatusPending {
				n = pending // not-yet-due rows
			}
			if n == 0 {
				continue
			}
			pct := 0.0
			if due > 0 {
				pct = float64(n) / float64(due) * 100
			}
			fmt.Fprintf(w, "  %-16s %8d   %5.1f%%\n", st, n, pct)
		}
		filled := counts[storage.MarkoutStatusFilled]
		fmt.Fprintf(w, "  %-16s %8d   %5.1f%%  (coverage: filled / due)\n",
			"coverage", filled, pctOf(filled, due))
		fmt.Fprintln(w)
	}
	return nil
}

func pctOf(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d) * 100
}
