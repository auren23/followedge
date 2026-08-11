// Command followedge is the FollowEdge CLI.
//
// v0.1.0-observe: collect smart-money/KOL trades into SQLite, detect wallet
// convergence clusters, sample forward markouts, and answer research
// questions (latency, chase, wallets, clusters). No trading.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/auren23/followedge/internal/analyze"
	"github.com/auren23/followedge/internal/cluster"
	"github.com/auren23/followedge/internal/collector"
	"github.com/auren23/followedge/internal/config"
	"github.com/auren23/followedge/internal/domain"
	"github.com/auren23/followedge/internal/markout"
	"github.com/auren23/followedge/internal/mechanism"
	"github.com/auren23/followedge/internal/source/gmgn"
	"github.com/auren23/followedge/internal/storage"
)

const version = "0.2.1.4-wallet-type-match-fix"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "collect":
		err = cmdCollect(ctx, os.Args[2:])
	case "status":
		err = cmdStatus(os.Args[2:])
	case "actors":
		err = cmdActors(ctx, os.Args[2:])
	case "analyze":
		err = cmdAnalyze(ctx, os.Args[2:])
	case "version":
		fmt.Println("followedge", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `followedge %s — profit actor discovery & replication research

usage:
  followedge collect [--config configs/observe.yaml] [--once]
  followedge status [--config configs/observe.yaml]
  followedge actors rank    [--config ...] [--since 24h] [--horizon 60s] [--limit 20] [--min-trades 3]
                             [--sort quality|replicability|pnl|copy-ev] [--frontier]
  followedge actors inspect [--config ...] <wallet> [--since 24h]
  followedge actors behavior [--config ...] <wallet> [--since 24h] [--horizon 5m]
  followedge actors matrix [--config ...] [--since 24h] [--horizon 5m] [--min-quality 30]
                             [--min-side-a-n 5] [--min-side-b-n 5] [--min-side-*-coverage 0.5]
                             [--max-coverage-gap 0.30] [--min-prevalence 0.40] [--min-separation 0.25]
  followedge analyze latency   [--config ...] [--since 1h]
  followedge analyze latency-ev [--config ...] [--horizon 5m] [--side buy] [--by-chase]
  followedge analyze chase     [--config ...] [--horizon 30s] [--side buy]
  followedge analyze clusters  [--config ...] [--window 60s] [--limit 20]
  followedge version
`, version)
}

// openStore loads config + key and opens the SQLite store.
func openStore(args []string) (*config.Config, *storage.Store, error) {
	fs := flag.NewFlagSet("common", flag.ExitOnError)
	cfgPath := fs.String("config", "configs/observe.yaml", "config file")
	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return nil, nil, err
	}
	store, err := storage.Open(cfg.DBPath)
	if err != nil {
		return nil, nil, err
	}
	return cfg, store, nil
}

func cmdCollect(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("collect", flag.ExitOnError)
	cfgPath := fs.String("config", "configs/observe.yaml", "config file")
	once := fs.Bool("once", false, "single poll cycle per source, then exit")
	_ = fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	key := config.GMGNKey()
	if key == "" {
		return fmt.Errorf("GMGN_API_KEY not set (env or ~/.config/gmgn/.env)")
	}
	store, err := storage.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer store.Close()

	client := gmgn.New(key, cfg.GMGN.BaseURL)
	limiter := collector.NewLimiter(cfg.GMGN.Limiter.WeightPerSecond, cfg.GMGN.Limiter.Burst)
	dedup := collector.NewDedup(10*time.Minute, 20000)

	windows, err := parseDurations(cfg.Cluster.Windows)
	if err != nil {
		return err
	}
	horizons, err := markout.ParseHorizons(cfg.Markout.Horizons)
	if err != nil {
		return err
	}

	clusterEngine := cluster.NewEngine(store, windows)
	markoutEngine := markout.NewEngine(store, client, limiter, cfg.Chain, cfg.Markout.Resolution, cfg.Markout.Grace, horizons)

	pipeline := func(ctx context.Context, e domain.TradeEvent) error {
		if e.PriceUSD > 0 {
			// leader rows: base = trade_time, entry = leader's price
			price := e.PriceUSD
			if err := store.CreateMarkouts(e, storage.MarkoutLeader, &price, horizons, time.Now().UTC()); err != nil {
				slog.Warn("create leader markouts failed", "event", e.ID, "err", err)
			}
			// follower rows: base = received_at, entry price sampled later —
			// this is the measurement that replicability/chase must use
			if err := store.CreateMarkouts(e, storage.MarkoutFollower, nil, horizons, time.Now().UTC()); err != nil {
				slog.Warn("create follower markouts failed", "event", e.ID, "err", err)
			}
		}
		if err := store.UpsertActor(e); err != nil {
			slog.Warn("actor upsert failed", "wallet", e.Wallet, "err", err)
		}
		return clusterEngine.OnEvent(ctx, e)
	}

	sources := []collector.Source{}
	if cfg.GMGN.SmartMoney.Enabled {
		sources = append(sources, &collector.GMGNSource{
			NameValue: "gmgn_smartmoney", WalletType: domain.WalletSmartMoney,
			Client: client, Chain: cfg.Chain, Limit: cfg.GMGN.SmartMoney.Limit,
		})
	}
	if cfg.GMGN.KOL.Enabled {
		sources = append(sources, &collector.GMGNSource{
			NameValue: "gmgn_kol", WalletType: domain.WalletKOL,
			Client: client, Chain: cfg.Chain, Limit: cfg.GMGN.KOL.Limit,
		})
	}
	if len(sources) == 0 {
		return fmt.Errorf("no collectors enabled in %s", *cfgPath)
	}

	// start the markout sampler in the background
	mctx, mstop := context.WithCancel(ctx)
	defer mstop()
	if !*once {
		go markoutEngine.Run(mctx, cfg.Markout.Tick)
	}

	for _, src := range sources {
		c := &collector.Collector{
			Source:   src,
			Interval: pollInterval(cfg, src),
			Store:    store,
			Limiter:  limiter,
			Dedup:    dedup,
			OnEvent:  pipeline,
			Print:    printTrade,
		}
		if *once {
			n, err := c.PollOnce(ctx)
			if err != nil {
				return err
			}
			slog.Info("poll done", "source", src.Name(), "new_events", n)
			continue
		}
		go c.Run(ctx)
	}

	if *once {
		return nil
	}
	slog.Info("collectors running", "chain", cfg.Chain, "db", cfg.DBPath)
	<-ctx.Done()
	return nil
}

func pollInterval(cfg *config.Config, src collector.Source) time.Duration {
	if src.Name() == "gmgn_kol" {
		return cfg.GMGN.KOL.PollInterval
	}
	return cfg.GMGN.SmartMoney.PollInterval
}

// printTrade renders the spec's human line: "SMART BUY wallet=A token=X amount=$3200".
func printTrade(e domain.TradeEvent) {
	kind := "SMART"
	if e.WalletType == domain.WalletKOL {
		kind = "KOL"
	}
	sym := e.TokenSymbol
	if sym == "" {
		sym = e.TokenAddress[:min(8, len(e.TokenAddress))]
	}
	fmt.Printf("%s %-4s wallet=%s token=%s amount=$%.0f age=%.1fs\n",
		kind, strings.ToUpper(string(e.Side)), shortAddr(e.Wallet), sym, e.AmountUSD,
		e.SourceAge().Seconds())
}

func shortAddr(a string) string {
	if len(a) <= 12 {
		return a
	}
	return a[:6] + "..." + a[len(a)-4:]
}

func cmdStatus(args []string) error {
	_, store, err := openStore(args)
	if err != nil {
		return err
	}
	defer store.Close()
	events, markouts, clusters, err := store.Counts()
	if err != nil {
		return err
	}
	fmt.Printf("followedge %s\n", version)
	fmt.Printf("events:    %d\n", events)
	fmt.Printf("markouts:  %d\n", markouts)
	fmt.Printf("clusters:  %d\n", clusters)
	return nil
}

func cmdActors(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("actors needs a subcommand: rank|inspect|behavior|matrix")
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("actors "+sub, flag.ExitOnError)
	cfgPath := fs.String("config", "configs/observe.yaml", "config file")
	sinceStr := fs.String("since", "24h", "lookback window (e.g. 1h, 24h, 7d)")
	horizon := fs.String("horizon", "60s", "reference markout horizon for replicability")
	limit := fs.Int("limit", 20, "max rows")
	minTrades := fs.Int("min-trades", 3, "min trades per actor")
	sortBy := fs.String("sort", "quality", "rank axis: quality|replicability|pnl|copy-ev")
	frontier := fs.Bool("frontier", false, "keep only the Pareto frontier on (quality, conservative EV)")
	noExitLoss := fs.Float64("noexit-loss", 100, "assumed loss %% for unpriced market-outcome rows in conservative EV")
	minReplMarket := fs.Int("min-repl-market", 20, "min effective market rows (filled + market loss) for frontier/replication sort eligibility")
	minReplFilled := fs.Int("min-repl-filled", 5, "min filled replication rows for frontier/replication sort eligibility")
	minQuality := fs.Float64("min-quality", 30, "target cohort quality gate (matrix)")
	minSideAN := fs.Int("min-side-a-n", 5, "matrix: min evaluable actors, discovery side A")
	minSideBN := fs.Int("min-side-b-n", 5, "matrix: min evaluable actors, comparison side B (P0 gate)")
	minSideACoverage := fs.Float64("min-side-a-coverage", 0.5, "matrix: min evaluable fraction of side A's cell")
	minSideBCoverage := fs.Float64("min-side-b-coverage", 0.5, "matrix: min evaluable fraction of side B's cell")
	maxCoverageGap := fs.Float64("max-coverage-gap", 0.30, "matrix: max |coverageA - coverageB|")
	minPrevalence := fs.Float64("min-prevalence", 0.40, "matrix: min prevalence on the discovery side")
	minSeparation := fs.Float64("min-separation", 0.25, "matrix: min prevalence separation vs the comparison side")
	if err := fs.Parse(rest); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	store, err := storage.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer store.Close()

	since, err := time.ParseDuration(*sinceStr)
	if err != nil {
		return fmt.Errorf("bad --since %q: %w", *sinceStr, err)
	}
	sinceT := time.Now().UTC().Add(-since)
	h, err := time.ParseDuration(*horizon)
	if err != nil {
		return fmt.Errorf("bad --horizon %q: %w", *horizon, err)
	}

	switch sub {
	case "rank":
		return analyze.Rank(os.Stdout, store, sinceT, h, *limit, *minTrades,
			*noExitLoss, cfg.Markout.Grace, analyze.ActorSortKey(*sortBy), *frontier,
			*minReplMarket, *minReplFilled)
	case "inspect":
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: followedge actors inspect <wallet> [--since 24h]")
		}
		horizons, err := markout.ParseHorizons(cfg.Markout.Horizons)
		if err != nil {
			return err
		}
		return analyze.Inspect(os.Stdout, store, fs.Arg(0), sinceT, horizons, *noExitLoss, cfg.Markout.Grace)
	case "behavior":
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: followedge actors behavior <wallet> [--since 24h]")
		}
		windows, err := parseDurations(cfg.Cluster.Windows)
		if err != nil {
			return err
		}
		clusterWindow := time.Minute
		if len(windows) > 0 {
			clusterWindow = windows[0]
		}
		return analyze.Behavior(os.Stdout, store, fs.Arg(0), sinceT, h,
			*noExitLoss, cfg.Markout.Grace, clusterWindow)
	case "matrix":
		windows, err := parseDurations(cfg.Cluster.Windows)
		if err != nil {
			return err
		}
		clusterWindow := time.Minute
		if len(windows) > 0 {
			clusterWindow = windows[0]
		}
		opts := mechanism.HypothesisOpts{
			MinSideAN:          *minSideAN,
			MinSideBN:          *minSideBN,
			MinSideAPrevalence: *minPrevalence,
			MinSeparation:      *minSeparation,
			MinSideACoverage:   *minSideACoverage,
			MinSideBCoverage:   *minSideBCoverage,
			MaxCoverageGap:     *maxCoverageGap,
			Window:             *sinceStr,
		}
		return analyze.Matrix(os.Stdout, store, sinceT, h, cfg.Markout.Grace,
			*noExitLoss, clusterWindow, *minReplMarket, *minReplFilled,
			*minQuality, opts)
	default:
		return fmt.Errorf("unknown actors subcommand %q", sub)
	}
}

func cmdAnalyze(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("analyze needs a subcommand: latency|chase|clusters")
	}
	sub, rest := args[0], args[1:]

	fs := flag.NewFlagSet("analyze "+sub, flag.ExitOnError)
	cfgPath := fs.String("config", "configs/observe.yaml", "config file")
	sinceStr := fs.String("since", "24h", "lookback window (e.g. 1h, 24h, 7d)")
	horizon := fs.String("horizon", "30s", "markout horizon (chase/latency-ev)")
	side := fs.String("side", "", "filter by side: buy|sell")
	byChase := fs.Bool("by-chase", false, "latency-ev: two-dimensional age × chase matrix")
	window := fs.String("window", "60s", "cluster window (clusters)")
	limit := fs.Int("limit", 20, "max rows (clusters)")
	if err := fs.Parse(rest); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	store, err := storage.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer store.Close()

	since, err := time.ParseDuration(*sinceStr)
	if err != nil {
		return fmt.Errorf("bad --since %q: %w", *sinceStr, err)
	}
	sinceT := time.Now().UTC().Add(-since)
	h, err := time.ParseDuration(*horizon)
	if err != nil {
		return fmt.Errorf("bad --horizon %q: %w", *horizon, err)
	}
	w, err := time.ParseDuration(*window)
	if err != nil {
		return fmt.Errorf("bad --window %q: %w", *window, err)
	}

	switch sub {
	case "latency":
		return analyze.Latency(os.Stdout, store, sinceT)
	case "latency-ev":
		noExitLoss := fs.Float64("noexit-loss", 100, "assumed loss %% for unpriced rows in conservative EV")
		return analyze.LatencyEV(os.Stdout, store, h, *side, *byChase, *noExitLoss, cfg.Markout.Grace)
	case "chase":
		return analyze.Chase(os.Stdout, store, h, *side)
	case "coverage":
		horizons, err := markout.ParseHorizons(cfg.Markout.Horizons)
		if err != nil {
			return err
		}
		return analyze.Coverage(os.Stdout, store, storage.MarkoutFollower, horizons, cfg.Markout.Grace)
	case "episodes":
		n, err := store.RebuildEpisodes(sinceT)
		if err != nil {
			return err
		}
		st, err := store.EpisodeStats()
		if err != nil {
			return err
		}
		fmt.Printf("POSITION EPISODES (since %s)\n", sinceT.Format("2006-01-02"))
		fmt.Printf("  total %d  (closed %d / open %d / partial %d)\n", st.Total, st.Closed, st.Open, st.Partial)
		if st.Closed > 0 {
			d := time.Duration(st.AvgHoldSecs) * time.Second
			fmt.Printf("  avg hold    %s\n", d.Round(time.Second).String())
			fmt.Printf("  total pnl   $%.2f  (%.0f%% profitable episodes)\n", st.TotalPnl, pctF(st.Profitable, st.Closed))
			fmt.Printf("  avg pnl     $%.2f per closed episode\n", st.AvgPnl)
		}
		fmt.Printf("  rebuilt %d episodes into position_episodes\n", n)
	case "clusters":
		return analyze.Clusters(os.Stdout, store, w, sinceT, *limit)
	default:
		return fmt.Errorf("unknown analyze subcommand %q", sub)
	}
	return nil
}

func pctF(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d) * 100
}

func parseDurations(specs []string) ([]time.Duration, error) {
	out := make([]time.Duration, 0, len(specs))
	for _, s := range specs {
		d, err := time.ParseDuration(s)
		if err != nil {
			return nil, fmt.Errorf("bad duration %q: %w", s, err)
		}
		out = append(out, d)
	}
	return out, nil
}

// small helpers to read analyze flags after openStore's Parse consumed part
// of the same args (flag sets are independent, so values may be lost).
// ponytail: single source of truth for these flags lives in the subcommand
// flag sets below; revisit if analyze grows more options.
