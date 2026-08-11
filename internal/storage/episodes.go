package storage

import (
	"database/sql"
	"fmt"
	"math"
	"time"
)

// Episode statuses.
const (
	EpisodeOpen    = "open"
	EpisodeClosed  = "closed"
	EpisodePartial = "partial" // sell legs exceeded visible position (data gap)
)

// EpisodeID is the lineage key connecting a ClassifiedEntry to the episode
// it belongs to. Format "wallet/token#ordinal" — deterministic over the
// event stream (never persisted; rebuilt on demand).
type EpisodeID string

// Episode is one reconstructed position round-trip of a (wallet, token).
//
// Status semantics (v0.2.0.1): "partial" means the sell legs EXCEEDED the
// visible position — a DATA GAP (our window missed the opening buys), NOT
// an intentional partial-exit behavior. Real partial exits are counted
// separately in PartialExitLegs: sell legs that left a positive visible
// quantity behind.
type Episode struct {
	// ID is assigned once at reconstruction; every entry of this episode
	// carries the same ID (v0.2.1.1 entry↔episode lineage).
	ID EpisodeID

	// IsReentry is fixed at reconstruction time over FULL history: whether
	// this token had a finalized episode BEFORE this one. Evidence policy
	// decides which episodes enter a statistic — it never redefines the
	// feature itself (a censored first episode still makes the second a
	// re-entry).
	IsReentry bool

	Wallet        string
	Token         string
	OpenedAt      int64
	ClosedAt      int64 // 0 while open
	Adds          int
	Reduces       int
	CapitalIn     float64
	CapitalOut    float64
	RealizedPnL   float64
	HoldDurationS int64
	Status        string

	// v0.2.0.1 behavior facts (reconstructed; not persisted in
	// position_episodes — behavior analysis never depends on the table).
	FirstSellAt     int64   // first sell leg time, 0 = never sold
	InitialBuyUSD   float64 // opening buy notional (0 if opening buy unseen)
	AddBuyUSD       float64 // total notional of add buys
	SellLegs        int     // total sell legs
	PartialExitLegs int     // sell legs that left visible qty > 0 (real partial exit)
	DataGap         bool    // opening buy (or part of the position) unseen

	// OriginQuality tracks what we KNOW about this episode's opening:
	//   Censored     — the collector may have missed an earlier position;
	//                  the first visible buy could be an add.
	//   VisibleZero  — our visible ledger reached zero, but that does NOT
	//                  prove the wallet's real balance is zero (a hidden
	//                  pre-dataset position may remain). Inferred, research
	//                  only.
	//   ConfirmedZero— independent evidence (future: on-chain balance = 0)
	//                  proves the position reached zero. NO current source
	//                  provides this, so it is never assigned yet.
	// Mechanism analysis must only consume initial-entry features of
	// OriginConfirmedZero episodes.
	OriginQuality OriginQuality
}

// BehaviorDataset is ONE wallet's full-history reconstruction in a single
// pass: episodes AND classified entries, guaranteed to agree because both
// come from one walker (v0.2.1.1 — they used to be two parallel walkers
// that could disagree).
//
// Entry evidence fields (OriginQuality, DataGap) are assigned at episode
// FINALIZE, not at the buy event: an entry can never claim an evidence
// level its episode later lost (e.g. a VisibleZero opening whose episode
// then hit an oversold gap and became DataGap). Never persisted — rebuilt
// on demand from trade_events.
type BehaviorDataset struct {
	Episodes []Episode
	Entries  []ClassifiedEntry
}

// OriginQuality rates the evidence behind an episode's opening buy.
type OriginQuality int

const (
	OriginCensored OriginQuality = iota
	OriginVisibleZero
	OriginConfirmedZero
)

// RebuildEpisodes reconstructs ALL wallets' position episodes since `since`
// and materializes them into position_episodes.
//
// LEGACY/DEBUG CACHE: left-truncated at `since` and only used by the
// `analyze episodes` debug command. Mechanism analysis MUST use the
// on-demand ReconstructEpisodesFor / ReconstructBehaviorFor (full history +
// cohort filter) — never this table. Deterministic and idempotent (table
// wiped and rebuilt). Same-second trades are ordered by received_at then
// event_id (approximate without a chain tx index).
func (s *Store) RebuildEpisodes(since time.Time) (int, error) {
	rows, err := s.db.Query(`
		SELECT wallet, token_address, side, token_amount, amount_usd,
		       buy_cost_usd, trade_time, received_at, event_id
		FROM trade_events
		WHERE trade_time >= ?
		ORDER BY wallet, token_address, trade_time, received_at, event_id`, since.Unix())
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	ds, err := reconstructBehavior(rows)
	if err != nil {
		return 0, err
	}
	eps := ds.Episodes

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM position_episodes"); err != nil {
		return 0, err
	}
	for _, e := range eps {
		var closed any
		if e.ClosedAt > 0 {
			closed = e.ClosedAt
		}
		var hold any
		if e.HoldDurationS > 0 {
			hold = e.HoldDurationS
		}
		if _, err := tx.Exec(`
			INSERT INTO position_episodes
			(wallet, token, opened_at, closed_at, adds, reduces,
			 capital_in, capital_out, realized_pnl, hold_duration_s, status)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			e.Wallet, e.Token, e.OpenedAt, closed, e.Adds, e.Reduces,
			e.CapitalIn, e.CapitalOut, e.RealizedPnL, hold, e.Status); err != nil {
			return 0, fmt.Errorf("insert episode: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(eps), nil
}

// ReconstructBehaviorFor rebuilds ONE wallet's episodes AND classified
// entries in a single pass over full history (v0.2.1.1). This is the single
// source of truth for behavior analysis — ReconstructEpisodesFor and
// ClassifiedEntries are thin views over it, so episode evidence and entry
// evidence can never disagree.
//
// Left-truncation guard: reconstruction runs over the wallet's FULL history
// and only the final episodes (opened_at >= since) are returned by
// ReconstructEpisodesFor — an episode opened before the analysis window
// keeps its real InitialBuyUSD / OpenedAt instead of mistaking the first
// visible add for the opening buy.
func (s *Store) ReconstructBehaviorFor(wallet string) (*BehaviorDataset, error) {
	rows, err := s.db.Query(`
		SELECT wallet, token_address, side, token_amount, amount_usd,
		       buy_cost_usd, trade_time, received_at, event_id
		FROM trade_events
		WHERE wallet = ?
		ORDER BY token_address, trade_time, received_at, event_id`, wallet)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return reconstructBehavior(rows)
}

// ReconstructEpisodesFor returns the wallet's episodes whose OPENING is
// inside the analysis window (full-history reconstruction, then cohort
// filter). See ReconstructBehaviorFor.
func (s *Store) ReconstructEpisodesFor(wallet string, since time.Time) ([]Episode, error) {
	ds, err := s.ReconstructBehaviorFor(wallet)
	if err != nil {
		return nil, err
	}
	out := ds.Episodes[:0]
	for _, e := range ds.Episodes {
		if e.OpenedAt >= since.Unix() {
			out = append(out, e)
		}
	}
	return out, nil
}

// PositionState is what a sell leg left behind.
type PositionState int

const (
	Remaining PositionState = iota // qty > eps: position still open
	Closed                         // |qty| <= eps: full close, snapped to zero
	Oversold                       // qty < -eps: sold more than visible (data gap)
)

// positionBook tracks a (wallet, token) visible quantity with a RELATIVE
// epsilon (1e-9 of peak): float64 residue on large meme-token counts must
// not flip a full close into a data gap. Single source of truth for both
// episode reconstruction and entry classification — the two must never
// disagree on whether a position is open.
type positionBook struct {
	qty, peak float64
}

func (b *positionBook) eps() float64 {
	if b.peak <= 0 {
		return 1e-9
	}
	return math.Max(1e-9, b.peak*1e-9)
}

func (b *positionBook) add(amount float64) {
	b.qty += amount
	if b.qty > b.peak {
		b.peak = b.qty
	}
}

// sell subtracts and reports the resulting state. A full close RESETS the
// peak — otherwise the next episode would inherit the previous episode's
// epsilon and small opening buys would look empty (misclassified as
// initial when they are adds).
func (b *positionBook) sell(amount float64) PositionState {
	b.qty -= amount
	switch {
	case math.Abs(b.qty) <= b.eps():
		b.qty, b.peak = 0, 0
		return Closed
	case b.qty < -b.eps():
		return Oversold
	}
	if b.qty > b.peak {
		b.peak = b.qty
	}
	return Remaining
}

func (b *positionBook) isEmpty() bool { return b.qty <= b.eps() }
func (b *positionBook) reset()        { b.qty, b.peak = 0, 0 }

// reconstructBehavior is the single walker behind ALL behavior
// reconstruction (episodes + classified entries), replacing the two
// parallel walkers that previously disagreed on evidence. Runs over rows
// ordered by (wallet, token_address, trade_time, received_at, event_id).
//
// Evidence is assigned in two stages:
//   - the RUNNING origin (Censored → VisibleZero on a visible full close,
//     back to Censored on an oversold gap) seeds the NEXT episode;
//   - every episode's entries inherit the episode's FINAL OriginQuality and
//     DataGap at finalize — a VisibleZero opening whose episode later goes
//     oversold becomes DataGap for its entries too, so episode stats and
//     entry stats can never claim different evidence for the same position.
func reconstructBehavior(rows *sql.Rows) (*BehaviorDataset, error) {
	ds := &BehaviorDataset{}
	seen := map[string]bool{} // wallet-level: tokens with >= 1 finalized episode
	var cur *Episode
	var curEntries []*ClassifiedEntry
	var curWallet, curToken string
	book := &positionBook{}
	origin := OriginCensored // per token group: quality of the NEXT episode's opening
	seq := 0                 // episode ordinal within the (wallet, token) group

	finalize := func() {
		if cur == nil {
			return
		}
		seen[cur.Token] = true
		for _, ce := range curEntries {
			// entry ↔ episode lineage: the final evidence, not the running
			// origin at buy time — a later oversold downgrades everything
			// this episode emitted.
			ce.OriginQuality = cur.OriginQuality
			ce.DataGap = cur.DataGap
			ce.EpisodeID = cur.ID
			ds.Entries = append(ds.Entries, *ce)
		}
		ds.Episodes = append(ds.Episodes, *cur)
		cur, curEntries = nil, nil
		book.reset()
	}

	for rows.Next() {
		var wallet, token, side string
		var tokenAmount, amountUSD, ts, received float64
		var buyCost sql.NullFloat64
		var eventID string
		if err := rows.Scan(&wallet, &token, &side, &tokenAmount, &amountUSD,
			&buyCost, &ts, &received, &eventID); err != nil {
			return nil, err
		}
		if cur == nil || wallet != curWallet || token != curToken {
			finalize()
			if wallet != curWallet || token != curToken {
				origin = OriginCensored // new (wallet, token) group: censored again
				book.reset()
				seq = 0
			}
			curWallet, curToken = wallet, token
			seq++
			cur = &Episode{
				Wallet: wallet, Token: token, OpenedAt: int64(ts), Status: EpisodeOpen,
				ID:            EpisodeID(fmt.Sprintf("%s/%s#%d", wallet, token, seq)),
				IsReentry:     seen[token],
				OriginQuality: origin,
				DataGap:       side == "sell", // window opened on a sell: opening buy unseen
			}
			if side == "sell" {
				// A visible full close only upgrades to VisibleZero — it cannot
				// prove the wallet's real balance is zero (hidden pre-dataset
				// position), so OriginConfirmedZero is never reached with the
				// current data sources.
				cur.OriginQuality = OriginCensored // opening buy unseen
			}
		}
		if side == "buy" {
			initial := book.isEmpty()
			if initial {
				// opening buy (or re-opening after a full close)
				cur.OpenedAt = int64(ts)
				cur.InitialBuyUSD = amountUSD
				cur.DataGap = false
				cur.FirstSellAt = 0
			} else {
				cur.Adds++
				cur.AddBuyUSD += amountUSD
			}
			book.add(tokenAmount)
			cur.CapitalIn += amountUSD
			curEntries = append(curEntries, &ClassifiedEntry{
				EventID: eventID, Token: token,
				TokenAmount: tokenAmount, AmountUSD: amountUSD,
				TradeTime: int64(ts), ReceivedAt: int64(received),
				Initial:          initial,
				SinceInitialSecs: int64(ts) - cur.OpenedAt,
			})
		} else { // sell
			if book.isEmpty() {
				cur.DataGap = true // selling with no visible position
			}
			if cur.FirstSellAt == 0 {
				cur.FirstSellAt = int64(ts)
			}
			cur.SellLegs++
			cur.Reduces++
			cur.CapitalOut += amountUSD
			if buyCost.Valid && buyCost.Float64 > 0 {
				cur.RealizedPnL += amountUSD - buyCost.Float64
			}
			switch book.sell(tokenAmount) {
			case Remaining:
				cur.PartialExitLegs++ // position remains: a REAL partial exit
			case Closed:
				cur.Status = EpisodeClosed
				cur.ClosedAt = int64(ts)
				cur.HoldDurationS = cur.ClosedAt - cur.OpenedAt
				// Our ledger reached zero — inferred, NOT confirmed: a hidden
				// pre-dataset position could still be open.
				origin = OriginVisibleZero
				finalize()
			case Oversold:
				cur.Status = EpisodePartial // sold more than visible position
				cur.DataGap = true
				cur.ClosedAt = int64(ts)
				cur.HoldDurationS = cur.ClosedAt - cur.OpenedAt
				// an oversold leg proves our trajectory has a gap — the
				// next episode's origin confidence drops back to Censored
				origin = OriginCensored
				finalize()
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// the last episode per group stays open
	finalize()
	return ds, nil
}

// EpisodeCounts aggregates episodes for analysis.
type EpisodeCounts struct {
	Total, Closed, Open, Partial int
	AvgHoldSecs                  int64
	TotalPnl                     float64
	Profitable                   int
	AvgPnl                       float64
}

func (s *Store) EpisodeStats() (EpisodeCounts, error) {
	var c EpisodeCounts
	rows, err := s.db.Query(`
		SELECT status, COUNT(*), COALESCE(AVG(hold_duration_s),0),
		       COALESCE(SUM(realized_pnl),0),
		       SUM(CASE WHEN realized_pnl > 0 THEN 1 ELSE 0 END)
		FROM position_episodes GROUP BY status`)
	if err != nil {
		return c, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var n int
		var avgHold, sumPnl, wins float64
		if err := rows.Scan(&status, &n, &avgHold, &sumPnl, &wins); err != nil {
			return c, err
		}
		c.Total += n
		switch status {
		case EpisodeClosed:
			c.Closed += n
			c.AvgHoldSecs = int64(avgHold)
			c.AvgPnl = sumPnl / float64(n)
		case EpisodeOpen:
			c.Open += n
		case EpisodePartial:
			c.Partial += n
		}
		if status == EpisodeClosed {
			c.TotalPnl += sumPnl
			c.Profitable += int(wins)
		}
	}
	return c, rows.Err()
}
