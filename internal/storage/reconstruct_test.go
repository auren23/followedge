package storage

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/auren23/followedge/internal/domain"
)

// TestBehaviorLineageFinalEvidence pins the v0.2.1.1 P0 fix: entries inherit
// their episode's FINAL evidence, assigned at episode finalize. An opening
// BUY that looked VisibleZero must stop being research-eligible the moment
// its episode hits an oversold gap — episode stats and entry stats can never
// disagree on the same position.
//
// Timeline (the review trap):
//
//	visible zero → BUY → ADD → oversold
//
//	episode research eligible = false
//	opening BUY research eligible = false
//	ADD research eligible = false
func TestBehaviorLineageFinalEvidence(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	base := now.Add(-1 * time.Hour).Truncate(30 * time.Second)
	mk := func(id, side string, ts time.Time, qty float64) {
		ev := domain.TradeEvent{
			ID:     domain.EventID("sol", id, "W_LN", "TOKEN_LN", side),
			Source: "gmgn_smartmoney", Chain: "sol", TxHash: id,
			Wallet: "W_LN", WalletType: domain.WalletSmartMoney,
			TokenAddress: "TOKEN_LN", Side: domain.Side(side), AmountUSD: qty,
			TokenAmount: qty, PriceUSD: 1.0, BuyCostUSD: 0,
			TradeTime: ts, ReceivedAt: ts,
		}
		if _, err := s.InsertEvent(ev); err != nil {
			t.Fatal(err)
		}
	}
	// b1: first observed episode → Censored. s1 closes it → visible zero.
	// b2: new episode with VisibleZero origin; b3 is an ADD to it.
	// s2: oversold → episode 2 becomes a DATA GAP.
	// b4: post-gap episode must be Censored again.
	mk("b1", "buy", base, 100)
	mk("s1", "sell", base.Add(30*time.Second), 100)
	mk("b2", "buy", base.Add(60*time.Second), 50)
	mk("b3", "buy", base.Add(90*time.Second), 20)
	mk("s2", "sell", base.Add(120*time.Second), 999)
	mk("b4", "buy", base.Add(150*time.Second), 10)

	ds, err := s.ReconstructBehaviorFor("W_LN")
	if err != nil {
		t.Fatal(err)
	}
	if len(ds.Episodes) != 3 {
		t.Fatalf("episodes = %+v, want 3", ds.Episodes)
	}
	ep2 := ds.Episodes[1]
	if ep2.OriginQuality != OriginVisibleZero || !ep2.DataGap || ep2.Status != EpisodePartial {
		t.Errorf("episode 2 = origin %v gap %v status %s, want VisibleZero/gap/partial",
			ep2.OriginQuality, ep2.DataGap, ep2.Status)
	}
	if ep2.IsReentry != true {
		t.Errorf("episode 2 must be a re-entry (TOKEN_LN had an episode before), got IsReentry=false")
	}
	if last := ds.Episodes[2]; last.OriginQuality != OriginCensored {
		t.Errorf("post-oversold episode must be Censored, got %v", last.OriginQuality)
	}

	// entries: b2/b3 belong to episode 2 and inherit its FINAL evidence —
	// VisibleZero origin BUT DataGap=true, so neither is research-eligible.
	byID := map[string]ClassifiedEntry{}
	for _, ce := range ds.Entries {
		byID[ce.EventID] = ce
	}
	b2, ok := byID[domain.EventID("sol", "b2", "W_LN", "TOKEN_LN", "buy")]
	if !ok {
		t.Fatal("b2 missing from entries")
	}
	b3 := byID[domain.EventID("sol", "b3", "W_LN", "TOKEN_LN", "buy")]
	b1 := byID[domain.EventID("sol", "b1", "W_LN", "TOKEN_LN", "buy")]
	b4 := byID[domain.EventID("sol", "b4", "W_LN", "TOKEN_LN", "buy")]

	if !b2.Initial || b3.Initial {
		t.Errorf("b2/b3 classification = %v/%v, want initial/add", b2.Initial, b3.Initial)
	}
	if b2.EpisodeID != ep2.ID || b3.EpisodeID != ep2.ID {
		t.Errorf("b2/b3 lineage = %q/%q, want episode 2 id %q", b2.EpisodeID, b3.EpisodeID, ep2.ID)
	}
	if !b2.DataGap || !b3.DataGap {
		t.Errorf("b2/b3 must inherit DataGap=true (their episode went oversold): %+v %+v", b2, b3)
	}
	if b2.OriginQuality != OriginVisibleZero || b3.OriginQuality != OriginVisibleZero {
		t.Errorf("b2/b3 origin = %v/%v, want VisibleZero (origin kept, gap excludes)", b2.OriginQuality, b3.OriginQuality)
	}
	// research eligibility is origin ∈ {Visible, Confirmed} AND !DataGap —
	// b2/b3 keep the VisibleZero origin but their gap must exclude them.
	researchEligible := func(ce ClassifiedEntry) bool {
		return (ce.OriginQuality == OriginVisibleZero || ce.OriginQuality == OriginConfirmedZero) && !ce.DataGap
	}
	if researchEligible(b2) || researchEligible(b3) {
		t.Errorf("b2/b3 must NOT be research eligible (DataGap episode): %+v %+v", b2, b3)
	}
	if b1.OriginQuality != OriginCensored || b1.DataGap || b1.EpisodeID != ds.Episodes[0].ID {
		t.Errorf("b1 = origin %v gap %v ep %q, want Censored/complete/episode-1",
			b1.OriginQuality, b1.DataGap, b1.EpisodeID)
	}
	if b4.OriginQuality != OriginCensored || b4.EpisodeID != ds.Episodes[2].ID {
		t.Errorf("b4 = origin %v ep %q, want Censored/episode-3", b4.OriginQuality, b4.EpisodeID)
	}
}

// TestReentryFixedOverFullHistory pins the P1-high definition: IsReentry is
// fixed at full-history reconstruction (token had an episode BEFORE this
// one), so a re-entry from a CENSORED first episode still counts as a
// re-entry. The evidence policy decides which episodes enter a statistic's
// denominator — it never redefines the feature itself.
func TestReentryFixedOverFullHistory(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	base := now.Add(-1 * time.Hour).Truncate(30 * time.Second)
	mk := func(id, side string, ts time.Time, qty float64) {
		ev := domain.TradeEvent{
			ID:     domain.EventID("sol", id, "W_RE", "TOKEN_RE", side),
			Source: "gmgn_smartmoney", Chain: "sol", TxHash: id,
			Wallet: "W_RE", WalletType: domain.WalletSmartMoney,
			TokenAddress: "TOKEN_RE", Side: domain.Side(side), AmountUSD: qty,
			TokenAmount: qty, PriceUSD: 1.0, BuyCostUSD: 0,
			TradeTime: ts, ReceivedAt: ts,
		}
		if _, err := s.InsertEvent(ev); err != nil {
			t.Fatal(err)
		}
	}
	// TOKEN_RE episode #1: Censored (first observed — could be an add to a
	// hidden pre-dataset position). It still makes episode #2 a re-entry.
	mk("r1", "buy", base, 100)
	mk("r2", "sell", base.Add(30*time.Second), 100)
	mk("r3", "buy", base.Add(60*time.Second), 50)  // VisibleZero episode #2
	mk("r4", "sell", base.Add(90*time.Second), 50) // closes

	ds, err := s.ReconstructBehaviorFor("W_RE")
	if err != nil {
		t.Fatal(err)
	}
	if len(ds.Episodes) != 2 {
		t.Fatalf("episodes = %+v, want 2", ds.Episodes)
	}
	if ds.Episodes[0].OriginQuality != OriginCensored || ds.Episodes[0].IsReentry {
		t.Errorf("episode 1 = origin %v reentry %v, want Censored/non-reentry", ds.Episodes[0].OriginQuality, ds.Episodes[0].IsReentry)
	}
	// episode #2 is a RE-ENTRY even though episode #1 was censored (excluded
	// from the research channel): the feature lives on full history.
	if !ds.Episodes[1].IsReentry {
		t.Errorf("episode 2 must be IsReentry=true (token had an earlier episode, censored or not)")
	}
	if ds.Episodes[1].OriginQuality != OriginVisibleZero {
		t.Errorf("episode 2 origin = %v, want VisibleZero", ds.Episodes[1].OriginQuality)
	}
}
