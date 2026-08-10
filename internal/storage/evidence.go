package storage

import (
	"encoding/json"
	"fmt"
	"time"
)

// Evidence levels (E0..E4): how trustworthy an actor's profit claim is.
// Nothing below E3 is treated as ground truth.
const (
	EvidenceE0 = "E0" // social claim / self-report: not trusted
	EvidenceE1 = "E1" // GMGN feed derived (buy_cost_usd basis): candidate only
	EvidenceE2 = "E2" // trade reconstruction across many txs
	EvidenceE3 = "E3" // on-chain realized PnL verified (own tx parsing, v0.2+)
	EvidenceE4 = "E4" // long-period, cross-regime verified
)

var EvidenceRank = map[string]int{
	EvidenceE0: 0, EvidenceE1: 1, EvidenceE2: 2, EvidenceE3: 3, EvidenceE4: 4,
}

// UpsertEvidence records (or refreshes) one evidence row for an actor.
func (s *Store) UpsertEvidence(actorID, evidenceType, level, source string,
	periodStart, periodEnd time.Time, tradeCount int, realizedPnL *float64, metadata any) error {
	var meta []byte
	if metadata != nil {
		b, err := json.Marshal(metadata)
		if err != nil {
			return err
		}
		meta = b
	}
	var pnl any
	if realizedPnL != nil {
		pnl = *realizedPnL
	}
	_, err := s.db.Exec(`
		INSERT INTO actor_evidence
		(actor_id, evidence_type, level, source, period_start, period_end,
		 trade_count, realized_pnl, metadata_json, verified_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(actor_id, evidence_type, level, period_start, period_end)
		DO UPDATE SET trade_count = excluded.trade_count,
		              realized_pnl = excluded.realized_pnl,
		              metadata_json = excluded.metadata_json,
		              verified_at = excluded.verified_at`,
		actorID, evidenceType, level, source,
		periodStart.Unix(), periodEnd.Unix(),
		tradeCount, pnl, meta, time.Now().UTC().Unix())
	if err != nil {
		return fmt.Errorf("upsert evidence: %w", err)
	}
	return nil
}

// EvidenceRow is one persisted evidence record.
type EvidenceRow struct {
	ActorID     string
	Level       string
	Source      string
	PeriodStart int64
	PeriodEnd   int64
	TradeCount  int
	RealizedPnL *float64
	VerifiedAt  int64
}

// EvidenceFor returns all evidence rows of an actor, newest first.
func (s *Store) EvidenceFor(actorID string) ([]EvidenceRow, error) {
	rows, err := s.db.Query(`
		SELECT actor_id, level, source, period_start, period_end, trade_count,
		       realized_pnl, verified_at
		FROM actor_evidence WHERE actor_id = ?
		ORDER BY verified_at DESC, level DESC`, actorID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EvidenceRow
	for rows.Next() {
		var r EvidenceRow
		var pnl any
		if err := rows.Scan(&r.ActorID, &r.Level, &r.Source, &r.PeriodStart, &r.PeriodEnd,
			&r.TradeCount, &pnl, &r.VerifiedAt); err != nil {
			return nil, err
		}
		if f, ok := pnl.(float64); ok {
			r.RealizedPnL = &f
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
