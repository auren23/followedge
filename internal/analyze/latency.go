package analyze

import (
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/auren23/followedge/internal/storage"
)

// Latency prints the source-age distribution: how stale were trades when we
// received them? This decides whether REST polling is fast enough at all.
func Latency(w io.Writer, s *storage.Store, since time.Time) error {
	rows, err := s.RecentEventsAll(since)
	if err != nil {
		return err
	}
	byType := map[string][]float64{}
	for _, r := range rows {
		age := float64(r.ReceivedAt - r.TradeTime) // seconds
		byType[r.WalletType] = append(byType[r.WalletType], age)
	}
	types := make([]string, 0, len(byType))
	for t := range byType {
		types = append(types, t)
	}
	sort.Strings(types)

	fmt.Fprintf(w, "SOURCE AGE (seconds: TradeTime -> ReceivedAt)\n")
	fmt.Fprintf(w, "%-14s %8s %8s %8s %8s %8s %8s\n", "wallet_type", "N", "mean", "P50", "P90", "P95", "P99")
	for _, t := range types {
		a := byType[t]
		fmt.Fprintf(w, "%-14s %8d %8.1f %8.1f %8.1f %8.1f %8.1f\n",
			t, len(a), mean(a), percentile(a, 50), percentile(a, 90), percentile(a, 95), percentile(a, 99))
	}
	return nil
}
