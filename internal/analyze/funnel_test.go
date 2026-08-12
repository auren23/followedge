package analyze

import (
	"testing"

	"github.com/auren23/followedge/internal/mechanism"
)

// TestCMaturationFunnel pins the observability funnel (OBSERVABILITY
// exception; measurement frozen at 4e761e9). Hard invariants:
//
//	monotone: potential_c >= filled_ge_2 >= filled_ge_5 >= repl_eligible >= band_eligible
//	same-source: band_eligible == len(splitCells C)  (both classify the same
//	            rows with the same band — a second matching logic would break it)
//	drop reasons name the killing layer.
func TestCMaturationFunnel(t *testing.T) {
	mkActor := func(w, typ string, q, consEV float64, filled, marketLoss, trades int, consValid bool) *Actor {
		return &Actor{Wallet: w, WalletType: typ, Quality: q, ConsEV: consEV,
			Filled: filled, MarketLoss: marketLoss, Trades: trades, ConsValid: consValid}
	}
	actors := map[string]*Actor{
		"A1": mkActor("A1", "sm", 45, 5, 30, 0, 100, true),  // high quality — not potential C
		"C1": mkActor("C1", "sm", 25, 3, 40, 0, 100, true),  // potential C, 20/5 OK, in band → C
		"C2": mkActor("C2", "sm", 25, 3, 8, 0, 100, true),   // filled 8>=5 but effN 8<20 → fails gate (effN)
		"C3": mkActor("C3", "sm", 25, 3, 3, 0, 100, true),   // filled 3<5 and effN 3<20 → fails gate (both)
		"C4": mkActor("C4", "kol", 25, 3, 30, 0, 100, true), // 20/5 OK, type not in matchedA → drop type
		"C5": mkActor("C5", "sm", 25, 3, 30, 0, 10, true),   // 20/5 OK, trades below band → drop band
		"C6": mkActor("C6", "sm", 25, 3, 30, 0, 100, false), // not ConsValid → not potential C
		"B1": mkActor("B1", "sm", 45, -3, 30, 0, 100, true), // ConsEV<=0 → not potential C
	}
	replEligible := func(a *Actor) bool {
		return a.ConsValid && a.Filled+a.MarketLoss >= 20 && a.Filled >= 5
	}
	mkRow := func(w, typ string, q, consEV float64, trades int) mechanism.MechanismMatrixRow {
		return mechanism.MechanismMatrixRow{Wallet: w, WalletType: typ, Quality: q, ConsEV: consEV, Trades: trades}
	}
	// rows = the repl-eligible subset (exactly what Matrix feeds splitCells).
	rows := []mechanism.MechanismMatrixRow{
		mkRow("A1", "sm", 45, 5, 100),
		mkRow("C1", "sm", 25, 3, 100),
		mkRow("C4", "kol", 25, 3, 100),
		mkRow("C5", "sm", 25, 3, 10),
		mkRow("B1", "sm", 45, -3, 100),
	}

	// splitCells derives the band from cell A (A1: trades 100 → [50,200], {sm})
	// and must agree with the funnel's band layer by construction.
	a, b, c, d, _, lo, hi, bandTypes, _ := splitCells(rows, 30)
	if len(a) != 1 || len(b) != 1 || len(c) != 1 || len(d) != 0 {
		t.Fatalf("split = A%d B%d C%d D%d, want 1/1/1/0", len(a), len(b), len(c), len(d))
	}
	if c[0].Wallet != "C1" {
		t.Fatalf("cell C = %v, want [C1]", c)
	}

	f := computeCMaturationFunnel(rows, actors, replEligible, lo, hi, bandTypes, 30, 20, 5)
	f.FinalC = len(c)

	want := cMaturationFunnel{
		PotentialC:   5, // C1,C2,C3,C4,C5
		FilledGE2:    5, // all five have Filled >= 2
		FilledGE5:    4, // C1,C2,C4,C5 (C3 has 3)
		ReplEligible: 3, // C1,C4,C5
		DropFilled:   1, // C3
		DropEffN:     2, // C2,C3
		BandEligible: 1, // C1
		DropBand:     1, // C5 (10 < 50)
		DropType:     1, // C4 (kol)
		FinalC:       1,
		BandValid:    true,
	}
	if f != want {
		t.Errorf("funnel = %+v, want %+v", f, want)
	}

	// monotone pipeline + same-source invariant
	if !(f.PotentialC >= f.FilledGE2 && f.FilledGE2 >= f.FilledGE5 &&
		f.FilledGE5 >= f.ReplEligible && f.ReplEligible >= f.BandEligible) {
		t.Errorf("funnel not monotone: %+v", f)
	}
	if f.BandEligible != len(c) {
		t.Errorf("same-source violated: band_eligible %d != matrix C %d", f.BandEligible, len(c))
	}
}

// TestCMaturationFunnelNoCellA pins the degenerate path: no cell A → no band,
// splitCells buckets by label only, and every potential-C row lands in C.
func TestCMaturationFunnelNoCellA(t *testing.T) {
	actors := map[string]*Actor{
		"C1": {Wallet: "C1", WalletType: "sm", Quality: 25, ConsEV: 3, Filled: 30, MarketLoss: 0, Trades: 100, ConsValid: true},
		"B1": {Wallet: "B1", WalletType: "sm", Quality: 45, ConsEV: -3, Filled: 30, MarketLoss: 0, Trades: 100, ConsValid: true},
	}
	replEligible := func(a *Actor) bool { return a.ConsValid && a.Filled+a.MarketLoss >= 20 && a.Filled >= 5 }
	rows := []mechanism.MechanismMatrixRow{
		{Wallet: "C1", WalletType: "sm", Quality: 25, ConsEV: 3, Trades: 100},
		{Wallet: "B1", WalletType: "sm", Quality: 45, ConsEV: -3, Trades: 100},
	}
	a, b, c, d, _, _, _, bandTypes, note := splitCells(rows, 30)
	if note == "" || len(a) != 0 || len(b) != 1 || len(c) != 1 || len(d) != 0 {
		t.Fatalf("no-A split = A%d B%d C%d D%d note %q, want 0/1/1/0", len(a), len(b), len(c), len(d), note)
	}
	f := computeCMaturationFunnel(rows, actors, replEligible, 0, 0, bandTypes, 30, 20, 5)
	f.FinalC = len(c)
	if f.PotentialC != 1 || f.BandEligible != 1 || f.BandValid {
		t.Errorf("no-A funnel = %+v, want potential 1 / band 1 / BandValid=false", f)
	}
	if f.BandEligible != len(c) {
		t.Errorf("same-source violated (no-A path): %+v", f)
	}
}
