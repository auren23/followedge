package analyze

import (
	"fmt"
	"io"

	"github.com/auren23/followedge/internal/mechanism"
)

// C maturation funnel — OBSERVABILITY ONLY (measurement semantics frozen at
// 4e761e9; this is the "expose matrix cohort maturation funnel" exception).
//
// Counts how the potential-C population (Quality below the quality gate AND
// ConsEV > 0) shrinks through the FROZEN cohort pipeline:
//
//	potential_c → filled_ge_2/5 → repl_eligible (20/5) → band_eligible → matrix_c
//
// Hard constraints honored:
//  1. Default gate semantics are untouched — 20/5, quality gate, splitCells,
//     wallet-type set and activity band all stay exactly as the frozen code
//     defines them. Nothing here re-implements a "looks similar" condition:
//     the repl gate decision IS the matrix's own replEligible closure
//     (passed in), and the band layer runs over `rows` — the exact input to
//     splitCells — with the exact bandLo/bandHi/bandTypes splitCells
//     derived, so band_eligible == len(cell C) by construction.
//  2. The funnel participates in no decision — no gate, no hypothesis, no
//     A/B/C/D outcome, no REACHED. It only prints intermediate sets that
//     already exist, so a persistent C=0 can be attributed to replication
//     maturity vs activity/type matching instead of guessed at.
type cMaturationFunnel struct {
	PotentialC   int // ConsValid && Quality<gate && ConsEV>0
	FilledGE2    int // ... and Filled >= 2 (diagnostic fixed threshold)
	FilledGE5    int // ... and Filled >= minFilled (the gate's filled half)
	ReplEligible int // ... and the frozen 20/5 sample gate (replEligible closure)
	DropFilled   int // potential C failing the gate with Filled < minFilled
	DropEffN     int // potential C failing the gate with Filled+MarketLoss < minMarket
	BandEligible int // repl-eligible potential C that also pass band + type
	DropBand     int // ... trades outside the activity band
	DropType     int // ... wallet type not in the matched-A type set
	FinalC       int // the matrix's final C cell (== BandEligible by construction)
	BandValid    bool
}

// computeCMaturationFunnel walks the SAME data the matrix already has.
//
// replEligible must be the matrix's own closure (never re-implemented here);
// bandLo/bandHi/bandTypes are splitCells' own outputs. The band layer is
// evaluated over `rows` — splitCells' exact input — so the counts are the
// same set splitCells classifies into C, and the funnel's drop reasons can
// name which layer killed each candidate.
func computeCMaturationFunnel(rows []mechanism.MechanismMatrixRow, actors map[string]*Actor,
	replEligible func(*Actor) bool, bandLo, bandHi int, bandTypes map[string]bool,
	minQuality float64, minMarket, minFilled int) cMaturationFunnel {
	var f cMaturationFunnel
	for _, a := range actors {
		if !a.ConsValid || a.Quality >= minQuality || a.ConsEV <= 0 {
			continue
		}
		f.PotentialC++
		if a.Filled >= 2 {
			f.FilledGE2++
		}
		if a.Filled >= minFilled {
			f.FilledGE5++
		}
		if replEligible(a) {
			f.ReplEligible++
			continue
		}
		// The gate decision is replEligible(a) above; these two counters only
		// explain which half of the gate failed.
		if a.Filled < minFilled {
			f.DropFilled++
		}
		if a.Filled+a.MarketLoss < minMarket {
			f.DropEffN++
		}
	}
	// The band layer runs over `rows` (splitCells' exact input), filtered to
	// potential-C rows, with splitCells' own band/type set — the same set it
	// classifies into C.
	f.BandValid = bandTypes != nil
	for _, r := range rows {
		if r.Quality >= minQuality || r.ConsEV <= 0 {
			continue
		}
		if f.BandValid {
			switch {
			case r.Trades < bandLo || r.Trades > bandHi:
				f.DropBand++
			case !bandTypes[r.WalletType]:
				f.DropType++
			default:
				f.BandEligible++
			}
		} else {
			// No cell A: splitCells bucketed by label only (no band filter),
			// so every potential-C row lands in C.
			f.BandEligible++
		}
	}
	return f
}

func printCMaturationFunnel(w io.Writer, f cMaturationFunnel, minMarket, minFilled int) {
	replLabel := fmt.Sprintf("effectiveN>=%d && filled>=%d", minMarket, minFilled)
	fmt.Fprintf(w, "\nC MATURATION FUNNEL (observability only — measurement frozen at 4e761e9)\n")
	fmt.Fprintf(w, "  %-16s %-38s N=%d\n", "potential_c", "q<gate && ConsEV>0 (ConsValid)", f.PotentialC)
	fmt.Fprintf(w, "  %-16s %-38s N=%d\n", "filled_ge_2", "", f.FilledGE2)
	fmt.Fprintf(w, "  %-16s %-38s N=%d\n", "filled_ge_5", "", f.FilledGE5)
	fmt.Fprintf(w, "  %-16s %-38s N=%d\n", "repl_eligible", replLabel, f.ReplEligible)
	if f.BandValid {
		fmt.Fprintf(w, "  %-16s %-38s N=%d\n", "band_eligible", "repl + activity/type match", f.BandEligible)
	} else {
		fmt.Fprintf(w, "  %-16s %-38s N=%d (no cell A — band n/a)\n", "band_eligible", "repl + activity/type match", f.BandEligible)
	}
	fmt.Fprintf(w, "  %-16s %-38s N=%d\n", "matrix_c", "final cell C", f.FinalC)
	fmt.Fprintf(w, "  drop — repl gate: filled<%d: %d · effectiveN<%d: %d\n",
		minFilled, f.DropFilled, minMarket, f.DropEffN)
	if f.BandValid {
		fmt.Fprintf(w, "  drop — band: activity: %d · wallet type: %d\n", f.DropBand, f.DropType)
	}
	// same-source invariant: the band layer and cell C classify the same rows
	// with the same band, so they must agree.
	if f.BandEligible == f.FinalC {
		fmt.Fprintf(w, "  same-source check: band_eligible == matrix_c == %d ✓\n", f.FinalC)
	} else {
		fmt.Fprintf(w, "  same-source check: band_eligible %d != matrix_c %d — INVESTIGATE\n", f.BandEligible, f.FinalC)
	}
}
