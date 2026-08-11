package storage

import (
	"database/sql"
	"time"
)

// ClassifiedEntry is one buy event of a wallet, tagged with whether it is
// the episode's OPENING buy or an ADD, plus the seconds since the episode
// opened (adds only). This separates "why does he open" from "why does he
// add" — the two questions mechanism mining must not conflate.
//
// OriginQuality and DataGap are the episode's FINAL evidence, assigned at
// episode finalize by the unified reconstruction walker (v0.2.1.1): an
// entry can never claim an evidence level its episode later lost (a
// VisibleZero opening whose episode then hit an oversold gap is DataGap
// for its entries too, exactly as for the episode). Adds of a
// left-censored episode are just as untrustworthy as its opening buy.
type ClassifiedEntry struct {
	EventID          string
	Token            string
	EpisodeID        EpisodeID // lineage: which episode this buy belongs to
	TokenAmount      float64
	AmountUSD        float64
	TradeTime        int64
	ReceivedAt       int64
	Initial          bool  // opening buy of an episode vs an add
	SinceInitialSecs int64 // adds only: trade_time - episode opening time
	OriginQuality    OriginQuality
	DataGap          bool // episode's final trajectory gap (oversold / unseen opening)
}

// ClassifiedEntries classifies every buy of the wallet as initial or add,
// with the episode's FINAL evidence. Runs over full history so a window
// edge never mislabels an add as an opening buy. This is a thin view over
// the single unified walker (ReconstructBehaviorFor) — episode stats and
// entry stats are guaranteed to agree on evidence.
func (s *Store) ClassifiedEntries(wallet string) ([]ClassifiedEntry, error) {
	ds, err := s.ReconstructBehaviorFor(wallet)
	if err != nil {
		return nil, err
	}
	return ds.Entries, nil
}

// EntryObservation is the chase feature of ONE entry, knowable at entry
// time (follower entry price sampled) — deliberately NOT conditioned on the
// horizon markout being filled, and NOT tied to any horizon: chase is an
// event-level feature.
type EntryObservation struct {
	EventID   string
	TradeTime int64
	ChasePct  float64 // (entry price / leader price - 1) * 100
}

// EntryObservations returns one chase observation per buy entry of the
// wallet whose follower entry price was sampled (base_price NOT NULL).
// GROUP BY event: after SetFollowerEntry every horizon row of an event
// carries the same entry, so this is exactly one row per entry.
func (s *Store) EntryObservations(wallet string, since time.Time) ([]EntryObservation, error) {
	rows, err := s.db.Query(`
		SELECT e.event_id, e.trade_time, MIN(m.base_price), e.price_usd
		FROM markouts m
		JOIN trade_events e ON e.event_id = m.event_id
		WHERE m.kind = 'follower'
		  AND m.base_price IS NOT NULL
		  AND e.side = 'buy' AND e.wallet = ? AND e.trade_time >= ?
		GROUP BY e.event_id
		ORDER BY e.trade_time`, wallet, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EntryObservation
	for rows.Next() {
		var id string
		var ts int64
		var base, leader sql.NullFloat64
		if err := rows.Scan(&id, &ts, &base, &leader); err != nil {
			return nil, err
		}
		if base.Valid && leader.Valid && base.Float64 > 0 && leader.Float64 > 0 {
			out = append(out, EntryObservation{
				EventID:   id,
				TradeTime: ts,
				ChasePct:  (base.Float64/leader.Float64 - 1) * 100,
			})
		}
	}
	return out, rows.Err()
}

// PriorFlow counts distinct smart-money and KOL BUYERS of a token in
// [entryTime-window, entryTime) — the market context the actor saw BEFORE
// its entry, recomputed historically from trade_events.
//
// Valid=false when the window reaches before the dataset's first trade:
// \"0 prior buyers\" must not be fabricated from missing data.
type PriorFlow struct {
	Smart int
	KOL   int
	Valid bool
}

// DatasetStart returns the earliest trade_time in the database (0 when
// empty) — the lower bound beyond which prior-flow counts are unknowable.
func (s *Store) DatasetStart() (int64, error) {
	var start sql.NullInt64
	err := s.db.QueryRow(`SELECT MIN(trade_time) FROM trade_events`).Scan(&start)
	if err != nil {
		return 0, err
	}
	return start.Int64, nil
}

// PriorFlowAt counts distinct buyers in [entryTime-window, entryTime).
// The upper bound is STRICTLY < entryTime: without a chain tx index the
// same-second ordering is unknowable, so same-second trades are conserva-
// tively excluded (they could be the actor's own buy).
func (s *Store) PriorFlowAt(token string, entryTime int64, window time.Duration, datasetStart int64) (PriorFlow, error) {
	pf := PriorFlow{}
	if entryTime-window.Milliseconds()/1000 < datasetStart {
		return pf, nil // window reaches before the dataset: unknowable, not zero
	}
	err := s.db.QueryRow(`
		SELECT COUNT(DISTINCT CASE WHEN wallet_type = 'smart_money' AND side = 'buy' THEN wallet END),
		       COUNT(DISTINCT CASE WHEN wallet_type = 'kol' AND side = 'buy' THEN wallet END)
		FROM trade_events
		WHERE token_address = ? AND trade_time >= ? AND trade_time < ?`,
		token, entryTime-window.Milliseconds()/1000, entryTime).
		Scan(&pf.Smart, &pf.KOL)
	if err == sql.ErrNoRows {
		return pf, nil
	}
	if err != nil {
		return pf, err
	}
	pf.Valid = true
	return pf, nil
}
