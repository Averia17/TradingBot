package paper_memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"time"

	"tradingbot/trading-api/internal/accounts"
)

type Funding struct {
	CashUSD    string    `json:"cash_usd"`
	RecordedAt time.Time `json:"recorded_at"`
}

// ValuationState contains cash and held quantities known at a ledger cutoff.
type ValuationState struct {
	Funding        Funding
	Cutoff         time.Time
	AccountVersion int64
	LedgerSHA256   string
	LastEventID    string
	CashUSD        string
	FeesUSD        string
	Quantities     map[accounts.Instrument]*big.Int
}

// WalkValuations replays a single account once, visiting ascending cutoffs.
// Each hash covers compact Event JSON followed by a newline for every event in
// its account-version prefix. RecordedAt is normalized to UTC before hashing.
func WalkValuations(ctx context.Context, source EventReader, accountID string, cutoffs []time.Time, visit func(ValuationState) error) error {
	digest := sha256.New()
	var lastEventID string
	return walkSnapshots(ctx, source, accountID, cutoffs, func(event accounts.Event) error {
		event.RecordedAt = event.RecordedAt.UTC()
		lastEventID = event.ID
		data, err := json.Marshal(event)
		if err != nil {
			return err
		}
		digest.Write(data)
		digest.Write([]byte{'\n'})
		return nil
	}, func(p *projection, cutoff time.Time) error {
		quantities := make(map[accounts.Instrument]*big.Int, len(p.lots))
		for instrument, lots := range p.lots {
			total, remaining := new(big.Int), new(big.Int)
			for _, l := range lots {
				total.Add(total, remaining.Sub(l.quantity, &l.consumed))
			}
			if total.Sign() > 0 {
				quantities[instrument] = total
			}
		}
		return visit(ValuationState{
			Funding: p.funding, Cutoff: cutoff, AccountVersion: p.version,
			LedgerSHA256: hex.EncodeToString(digest.Sum(nil)), LastEventID: lastEventID,
			CashUSD: formatUSD(&p.cash), FeesUSD: formatUSD(&p.fees), Quantities: quantities,
		})
	})
}
