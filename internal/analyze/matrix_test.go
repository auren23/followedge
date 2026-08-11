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

// ---- v0.2.1.2 trap tests: runContrast / splitCells (analyze layer) ----

// mkRow builds a matrix row with the given chase (all other features present
// but irrelevant to the chase patterns under test) and 10 research episodes.
func mkChaseRow(wallet string, chase float64) mechanism.MechanismMatrixRow {
	return mechanism.MechanismMatrixRow{
		Wallet:       wallet,
		InitialChase: mechanism.MedianStat{Value: chase, N: 5},
		OriginCounts: mechanism.EvidenceCounts{Visible: 10},
	}
}

var momentum = mechanism.Pattern{
	Name:       "momentum chase entry",
	Conditions: []mechanism.FeatureCondition{{Feature: mechanism.FeatureInitialChase, Op: ">", A: 5}},
}

var addDelayOnly = mechanism.Pattern{
	Name:       "adds quickly",
	Conditions: []mechanism.FeatureCondition{{Feature: mechanism.FeatureAddDelay, Op: "between", A: 30, B: 120}},
}

// TestContrastCLIP0ControlZero is trap T1b (CLI half): a comparison side with
// zero evaluable actors renders the canonical un-eval format and NEVER
// graduates — missing control evidence is not 0% prevalence.
func TestContrastCLIP0ControlZero(t *testing.T) {
	var sideA, sideB []mechanism.MechanismMatrixRow
	for i := 0; i < 10; i++ {
		sideA = append(sideA, mkChaseRow(tWallet(i), 8)) // all match momentum
	}
	for i := 0; i < 20; i++ {
		r := mkChaseRow(cWallet(i), 8)
		r.InitialChase = mechanism.MedianStat{} // feature missing → not evaluable
		sideB = append(sideB, r)
	}
	var buf bytes.Buffer
	hyps := runContrast(&buf, "PROFIT", "C", "profit", sideA, sideB, []mechanism.Pattern{momentum}, mechanism.DefaultHypothesisOpts())
	out := buf.String()
	want := "matched 0/0 · evaluable 0/20 · total 20 · cov 0% (un-eval 20/20: 0 data · 20 unobservable)"
	if !strings.Contains(out, want) {
		t.Errorf("side B cell wrong, want %q in:\n%s", want, out)
	}
	if strings.Contains(out, "no-add-evidence") {
		t.Errorf("chase-only pattern must render unobservable, not no-add-evidence:\n%s", out)
	}
	if len(hyps) != 0 {
		t.Errorf("zero-evaluable control must NOT graduate, got %+v", hyps)
	}
}

// TestContrastCLIEmptySide is trap T5b: an empty contrast side prints the
// not-evaluable note and skips the pattern table + gateStatus annotations.
func TestContrastCLIEmptySide(t *testing.T) {
	var sideA []mechanism.MechanismMatrixRow
	for i := 0; i < 10; i++ {
		sideA = append(sideA, mkChaseRow(tWallet(i), 8))
	}
	var buf bytes.Buffer
	hyps := runContrast(&buf, "COPYABILITY", "B", "copyability", sideA, nil, []mechanism.Pattern{momentum}, mechanism.DefaultHypothesisOpts())
	out := buf.String()
	if !strings.Contains(out, "(contrast not evaluable: cell B is empty — 0 rows matched the A band)") {
		t.Errorf("empty-side note missing:\n%s", out)
	}
	if strings.Contains(out, "PATTERNS") || len(hyps) != 0 {
		t.Errorf("empty side must skip the pattern table and produce no hypotheses:\n%s", out)
	}
}

// TestContrastCLIDataVsAbsent is trap T10: the two kinds of unevaluable
// missingness are visibly distinct — data-missing (no research episodes) vs
// absent (research episodes but the add-family feature unobservable), and
// the absent label is honest for an add-family-only pattern.
func TestContrastCLIDataVsAbsent(t *testing.T) {
	var sideA, sideB []mechanism.MechanismMatrixRow
	sideA = append(sideA, mechanism.MechanismMatrixRow{
		Wallet: tWallet(0), AddDelay: mechanism.MedianStat{Value: 60, N: 5}, OriginCounts: mechanism.EvidenceCounts{Visible: 10}})
	sideA = append(sideA, mechanism.MechanismMatrixRow{
		Wallet: tWallet(1), AddDelay: mechanism.MedianStat{Value: 200, N: 5}, OriginCounts: mechanism.EvidenceCounts{Visible: 10}})
	sideA = append(sideA, mechanism.MechanismMatrixRow{Wallet: tWallet(2)}) // no research episodes at all
	sideA = append(sideA, mechanism.MechanismMatrixRow{
		Wallet: tWallet(3), OriginCounts: mechanism.EvidenceCounts{Visible: 10}}) // episodes but no add evidence
	for i := 0; i < 5; i++ {
		sideB = append(sideB, mechanism.MechanismMatrixRow{
			Wallet: cWallet(i), AddDelay: mechanism.MedianStat{Value: 200, N: 5}, OriginCounts: mechanism.EvidenceCounts{Visible: 10}})
	}
	var buf bytes.Buffer
	runContrast(&buf, "PROFIT", "C", "profit", sideA, sideB, []mechanism.Pattern{addDelayOnly}, mechanism.DefaultHypothesisOpts())
	out := buf.String()
	want := "matched 1/2 · evaluable 2/4 · total 4 · cov 50% (un-eval 2/4: 1 data · 1 no-add-evidence)"
	if !strings.Contains(out, want) {
		t.Errorf("side A cell wrong, want %q in:\n%s", want, out)
	}
}

// TestContrastCLIChaseLabelNeverAbsent is trap T11: a coverage-blocked
// chase-only pattern renders "unobservable" — the word "absent" must never
// appear, so the output can never imply "cell C doesn't chase". The test
// drives gateStatus with MinSideAN=0 so gate coverage-a is genuinely the
// first failing gate under the test's own opts.
func TestContrastCLIChaseLabelNeverAbsent(t *testing.T) {
	var sideA, sideB []mechanism.MechanismMatrixRow
	sideA = append(sideA, mkChaseRow(tWallet(0), 8))                        // evaluable, matches
	sideA = append(sideA, mechanism.MechanismMatrixRow{Wallet: tWallet(1)}) // data-missing
	for i := 2; i < 4; i++ {
		r := mkChaseRow(tWallet(i), 8)
		r.InitialChase = mechanism.MedianStat{} // research episodes but chase unobservable
		sideA = append(sideA, r)
	}
	for i := 0; i < 5; i++ {
		sideB = append(sideB, mkChaseRow(cWallet(i), 2)) // evaluable, not > 5
	}
	opts := mechanism.DefaultHypothesisOpts()
	opts.MinSideAN = 0 // isolate the coverage gate (defaults would fire side-a-n first)
	var buf bytes.Buffer
	runContrast(&buf, "PROFIT", "C", "profit", sideA, sideB, []mechanism.Pattern{momentum}, opts)
	out := buf.String()
	want := "matched 1/1 · evaluable 1/4 · total 4 · cov 25% (un-eval 3/4: 1 data · 2 unobservable)"
	if !strings.Contains(out, want) {
		t.Errorf("side A cell wrong, want %q in:\n%s", want, out)
	}
	if !strings.Contains(out, "coverage-a") {
		t.Errorf("first failing gate must be coverage-a under opts.MinSideAN=0:\n%s", out)
	}
	if strings.Contains(out, "absent") {
		t.Errorf("the word 'absent' must never print for a chase-only pattern:\n%s", out)
	}
}

// TestSplitCellsPartition is trap T6: the Quality × Replicability 2×2
// partition with the activity band applied to every cell, no row dropped,
// none duplicated, and the boundary rule pinned (Quality == gate → high row;
// ConsEV == 0 → right column).
func TestSplitCellsPartition(t *testing.T) {
	mk := func(wallet string, q, consEV float64) mechanism.MechanismMatrixRow {
		return mechanism.MechanismMatrixRow{
			Wallet: wallet, WalletType: "sm", Quality: q, ConsEV: consEV, Trades: 10,
		}
	}
	var rows []mechanism.MechanismMatrixRow
	for i := 0; i < 6; i++ {
		rows = append(rows, mk(tWallet(i), 50, 5)) // A
	}
	for i := 0; i < 4; i++ {
		rows = append(rows, mk(tWallet(10+i), 50, -1)) // B
	}
	for i := 0; i < 3; i++ {
		rows = append(rows, mk(tWallet(20+i), 20, 5)) // C
	}
	for i := 0; i < 9; i++ {
		rows = append(rows, mk(tWallet(30+i), 20, -1)) // D
	}
	a, b, c, d, dropped, lo, hi, note := splitCells(rows, 30)
	if len(a) != 6 || len(b) != 4 || len(c) != 3 || len(d) != 9 || dropped != 0 || note != "" {
		t.Errorf("split = A%d B%d C%d D%d dropped %d note %q, want 6/4/3/9/0", len(a), len(b), len(c), len(d), dropped, note)
	}
	if lo != 5 || hi != 20 { // A median 10 → band [5, 20]
		t.Errorf("band = %d-%d, want 5-20", lo, hi)
	}
	if len(a)+len(b)+len(c)+len(d)+dropped != len(rows) {
		t.Errorf("partition lost rows: %d", len(a)+len(b)+len(c)+len(d)+dropped)
	}

	// boundary: Quality == gate → high row; ConsEV == 0 → right column
	rows = []mechanism.MechanismMatrixRow{
		mk("B_A", 30, 1), mk("B_B", 30, 0), mk("B_C", 29, 1), mk("B_D", 29, 0),
	}
	a, b, c, d, _, _, _, _ = splitCells(rows, 30)
	if len(a) != 1 || len(b) != 1 || len(c) != 1 || len(d) != 1 {
		t.Errorf("boundary split = A%d B%d C%d D%d, want 1/1/1/1", len(a), len(b), len(c), len(d))
	}
}

// TestSplitCellsBandAppliesToA is trap T12 (v0.2.1.3): the activity band
// must be applied to cell A ITSELF, not only to B/C/D. With raw A trades
// {10,100,100,100,1000} → median 100 → band [50,200], the 10- and 1000-trade
// A-label actors are dropped exactly like band outliers in the other cells —
// otherwise a pattern separation could be an activity difference in disguise.
func TestSplitCellsBandAppliesToA(t *testing.T) {
	mk := func(wallet string, q, consEV float64, trades int) mechanism.MechanismMatrixRow {
		return mechanism.MechanismMatrixRow{
			Wallet: wallet, WalletType: "sm", Quality: q, ConsEV: consEV, Trades: trades,
		}
	}
	rows := []mechanism.MechanismMatrixRow{
		mk("A1", 50, 5, 10),   // A-label, below band → dropped
		mk("A2", 50, 5, 100),  // A
		mk("A3", 50, 5, 100),  // A
		mk("A4", 50, 5, 100),  // A
		mk("A5", 50, 5, 1000), // A-label, above band → dropped
		mk("B1", 50, -1, 60),  // B, in band
		mk("B2", 50, -1, 300), // B, outside band → dropped
		mk("C1", 20, 5, 70),   // C, in band
		mk("C2", 20, 5, 5000), // C, outside band → dropped
		mk("D1", 20, -1, 80),  // D, in band
	}
	a, b, c, d, dropped, lo, hi, _ := splitCells(rows, 30)
	if lo != 50 || hi != 200 {
		t.Fatalf("band = %d-%d, want 50-200 (raw A median 100)", lo, hi)
	}
	if len(a) != 3 || len(b) != 1 || len(c) != 1 || len(d) != 1 || dropped != 4 {
		t.Errorf("split = A%d B%d C%d D%d dropped %d, want 3/1/1/1/4", len(a), len(b), len(c), len(d), dropped)
	}
	for _, r := range a {
		if r.Wallet == "A1" || r.Wallet == "A5" {
			t.Errorf("band outlier %s leaked into cell A: %+v", r.Wallet, a)
		}
	}
	wallets := map[string]bool{}
	for _, r := range a {
		wallets[r.Wallet] = true
	}
	for _, w := range []string{"A2", "A3", "A4"} {
		if !wallets[w] {
			t.Errorf("cell A missing %s: %+v", w, a)
		}
	}
}

// ---- TestMatrixCLI: end-to-end smoke (v0.2.1.2 output) ----

// TestMatrixCLI is the v0.2.1.2 end-to-end smoke: profitable+copyable
// actors vs losing ones must split into 2×2 cells, print the OUTCOME /
// EVIDENCE COVERAGE / FEATURES / PATTERNS / HYPOTHESES sections under both
// contrasts without erroring. Graduation itself is tested at the mechanism
// level; here we pin the plumbing.
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

	// 6 target (ConsEV > 0) + 4 control (ConsEV <= 0), all quality >= 0
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

	for _, want := range []string{
		"MECHANISM MATRIX", "OUTCOME 2×2", "EVIDENCE COVERAGE", "FEATURES",
		"CONTRAST: PROFIT", "CONTRAST: COPYABILITY", "PATTERNS", "HYPOTHESES",
		"confirmed  visible  censored  gap  research",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// cell split: A = 6 profitable+copyable, B = 4 profitable-not-copyable,
	// C/D empty → profit contrast not evaluable (C empty)
	re := regexp.MustCompile(`A\s+6\s+B\s+4`)
	if !re.MatchString(out) {
		t.Errorf("2×2 layout wrong:\n%s", out)
	}
	if !strings.Contains(out, "contrast not evaluable: cell C is empty") {
		t.Errorf("empty C cell must render the not-evaluable note:\n%s", out)
	}
	// copyability runs its pattern table but nothing graduates (B n=4 < 5)
	if !strings.Contains(out, "no pattern cleared the gates") {
		t.Errorf("copyability block must print the empty-gates message:\n%s", out)
	}
}

// TestBehaviorCardNaming is trap T9: the behavior card's second column is
// now `research` (StrictN/ResearchN semantics) with the four-bucket
// breakdown on a sub-line, and the string "inferred" never appears.
func TestBehaviorCardNaming(t *testing.T) {
	s, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	base := now.Add(-1 * time.Hour).Truncate(30 * time.Second)
	mk := func(id, side string, ts time.Time, qty float64) {
		ev := domain.TradeEvent{
			ID:     domain.EventID("sol", id, "W_NM", "TOKEN_NM", side),
			Source: "gmgn_smartmoney", Chain: "sol", TxHash: id,
			Wallet: "W_NM", WalletType: domain.WalletSmartMoney,
			TokenAddress: "TOKEN_NM", Side: domain.Side(side), AmountUSD: qty,
			TokenAmount: qty, PriceUSD: 1.0, BuyCostUSD: 0,
			TradeTime: ts, ReceivedAt: ts,
		}
		if _, err := s.InsertEvent(ev); err != nil {
			t.Fatal(err)
		}
	}
	// b1 (Censored) closes; b2 is a VisibleZero episode → research channel.
	mk("b1", "buy", base, 100)
	mk("s1", "sell", base.Add(30*time.Second), 100)
	mk("b2", "buy", base.Add(60*time.Second), 50)
	mk("s2", "sell", base.Add(90*time.Second), 50)

	var buf bytes.Buffer
	if err := Behavior(&buf, s, "W_NM", now.Add(-2*time.Hour), 5*time.Minute, 100, 0, time.Minute); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "confirmed   research") {
		t.Errorf("card header must read 'confirmed   research':\n%s", out)
	}
	if !strings.Contains(out, "initial buys") || !strings.Contains(out, "confirmed 0 · visible 1 · censored 1 · gap 0") {
		t.Errorf("initial buys line wrong (research column = StrictN/ResearchN, breakdown sub-line):\n%s", out)
	}
	if strings.Contains(out, "inferred") {
		t.Errorf("the string 'inferred' must not appear in the card:\n%s", out)
	}
}

func tWallet(i int) string { return "TARGET_WALLET_" + string(rune('A'+i)) }
func cWallet(i int) string { return "CONTROL_WALLET_" + string(rune('A'+i)) }
