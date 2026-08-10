package analyze

import (
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/auren23/followedge/internal/storage"
)

// Clusters summarizes the rolling convergence samples for one window:
// how often do N smart wallets coincide, and which tokens converge hardest.
func Clusters(w io.Writer, s *storage.Store, window time.Duration, since time.Time, limit int) error {
	rows, err := s.ClusterSamples(window, since)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Fprintf(w, "no cluster samples for window %v (collect first)\n", window)
		return nil
	}

	// distribution of distinct smart-buy wallets per sample
	dist := map[int]int{}
	maxByToken := map[string]int{}
	totalFlow := map[string]float64{}
	flowN := map[string]int{}
	for _, r := range rows {
		dist[r.SmartBuyWallets]++
		if r.SmartBuyWallets > maxByToken[r.TokenAddress] {
			maxByToken[r.TokenAddress] = r.SmartBuyWallets
		}
		totalFlow[r.TokenAddress] += r.NetFlowUSD
		flowN[r.TokenAddress]++
	}

	fmt.Fprintf(w, "CLUSTERS window=%v samples=%d (since %s)\n", window, len(rows), since.Format("2006-01-02 15:04"))
	fmt.Fprintf(w, "%-8s %8s\n", "smart_wallets", "samples")
	keys := make([]int, 0, len(dist))
	for k := range dist {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "%-8d %8d\n", k, dist[k])
	}

	tokens := make([]string, 0, len(maxByToken))
	for t := range maxByToken {
		tokens = append(tokens, t)
	}
	sort.Slice(tokens, func(i, j int) bool {
		if maxByToken[tokens[i]] != maxByToken[tokens[j]] {
			return maxByToken[tokens[i]] > maxByToken[tokens[j]]
		}
		return totalFlow[tokens[i]]/float64(flowN[tokens[i]]) > totalFlow[tokens[j]]/float64(flowN[tokens[j]])
	})
	if limit > 0 && len(tokens) > limit {
		tokens = tokens[:limit]
	}
	fmt.Fprintf(w, "\n%-46s %12s %14s\n", "token", "max_smart", "avg_net_flow")
	for _, t := range tokens {
		fmt.Fprintf(w, "%-46s %12d %+13.0f\n", t, maxByToken[t], totalFlow[t]/float64(flowN[t]))
	}
	return nil
}
