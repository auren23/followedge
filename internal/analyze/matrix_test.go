package analyze

import (
	"bytes"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/auren23/followedge/internal/domain"
	"github.com/auren23/followedge/internal/mechanism"
	"github.com/auren23/followedge/internal/storage"
)

// TestMatrixCLI is the v0.2.1.1 end-to-end smoke: 6 profitable+copyable
// actors vs 4 losing ones must split into TARGET/CONTROL cohorts, print the
// evidence coverage / feature / pattern tables, and reach the hypothesis
// section without erroring. Hypothesis GRADUATION itself is tested at the
// mechanism level (GenerateHypotheses) — here we pin the plumbing.
func TestMatrixCLI(t *testing.T) {
	s, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	base := now.Add(-1 * time.Hour).Truncate(30 * time.Second)

	// Each actor: buy 100 → sell 100 (Censored episode) → buy 50 → sell 50
	// (VisibleZero episode → research channel). Buys get follower markouts
	// filled with the actor's replication return.
	mk := func(wallet, id string, side domain.Side, ts time.Time, qty, buyCost float64) {
		ev := domain.TradeEvent{
			ID:     domain.EventID("sol", id, wallet, "TOK_"+wallet, string(side)),
			Source: "gmgn_smartmoney", Chain: "sol", TxHash: id,
			Wallet: wallet, WalletType: domain.WalletSmartMoney,
			TokenAddress: "TOK_" + wallet, Side: side, AmountUSD: qty,
			TokenAmount: qty, PriceUSD: 1.0, BuyCostUSD: buyCost,
			TradeTime: ts, ReceivedAt: ts,
		}
		if _, err := s.InsertEvent(ev); err != nil {
			t.Fatal(err)
		}
	}
	fill := func(wallet, id string, ts time.Time, retPct float64) {
		evID := domain.EventID("sol", id, wallet, "TOK_"+wallet, "buy")
		entry := 1.0
		if err := s.CreateMarkouts(domain.TradeEvent{ID: evID}, storage.MarkoutFollower, &entry,
			[]time.Duration{30 * time.Second}, now); err != nil {
			t.Fatal(err)
		}
		if err := s.FillMarkout(evID, storage.MarkoutFollower, 30*time.Second, 1.0+retPct/100, 0); err != nil {
			t.Fatal(err)
		}
	}

	// 6 target (ConsEV > 0) + 4 control (ConsEV <= 0)
	for i := 0; i < 6; i++ {
		w := "TARGET_ACTOR_" + string(rune('A'+i))
		mk(w, "b1", domain.Buy, base, 100, 0)
		mk(w, "s1", domain.Sell, base.Add(30*time.Second), 100, 100)
		mk(w, "b2", domain.Buy, base.Add(60*time.Second), 50, 0)
		mk(w, "s2", domain.Sell, base.Add(90*time.Second), 50, 50)
		fill(w, "b1", base, 5)
		fill(w, "b2", base.Add(60*time.Second), 5)
	}
	for i := 0; i < 4; i++ {
		w := "CTRL_ACTOR_" + string(rune('A'+i))
		mk(w, "b1", domain.Buy, base, 100, 0)
		mk(w, "s1", domain.Sell, base.Add(30*time.Second), 100, 100)
		mk(w, "b2", domain.Buy, base.Add(60*time.Second), 50, 0)
		mk(w, "s2", domain.Sell, base.Add(90*time.Second), 50, 50)
		fill(w, "b1", base, -5)
		fill(w, "b2", base.Add(60*time.Second), -5)
	}

	opts := mechanism.DefaultHypothesisOpts()
	opts.Window = "1h"
	var buf bytes.Buffer
	if err := Matrix(&buf, s, now.Add(-2*time.Hour), 30*time.Second, 0, 100,
		time.Minute, 1, 1, 0, opts); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	for _, want := range []string{"MECHANISM MATRIX", "COHORTS", "EVIDENCE COVERAGE", "FEATURES", "PATTERNS", "HYPOTHESES"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// cohort split: 6 target / 4 control, each with 4 trades (the band base)
	re := regexp.MustCompile(`TARGET\s+6\s+4\s`)
	if !re.MatchString(out) {
		t.Errorf("target cohort row wrong:\n%s", out)
	}
	if !strings.Contains(out, "CONTROL") || !strings.Contains(out, "no pattern cleared the gates") {
		t.Errorf("control cohort / hypothesis gate output wrong:\n%s", out)
	}
}
