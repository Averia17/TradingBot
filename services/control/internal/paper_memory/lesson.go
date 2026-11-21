package paper_memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"tradingbot/control/internal/ledger"
)

// BuildLesson derives a bounded, versioned lesson from a realized SELL.
// The source digest includes the original FIFO lot allocations, while the
// persisted lesson stores only the exact amounts and linked identities.
func BuildLesson(ctx context.Context, source EventReader, accountID, fillEventID string, asOf time.Time) (ledger.LessonFact, error) {
	view, err := Load(ctx, source, accountID, asOf)
	if err != nil {
		return ledger.LessonFact{}, err
	}
	for _, outcome := range view.Realized {
		if outcome.FillEventID == fillEventID {
			return lessonFact(accountID, outcome)
		}
	}
	return ledger.LessonFact{}, ledger.ErrNotFound
}

func lessonFact(accountID string, outcome Outcome) (ledger.LessonFact, error) {
	source := struct {
		AccountID      string  `json:"account_id"`
		AccountVersion int64   `json:"account_version"`
		Method         string  `json:"accounting_method"`
		Outcome        Outcome `json:"outcome"`
	}{accountID, outcome.accountVersion, accountingMethod, outcome}
	raw, err := json.Marshal(source)
	if err != nil {
		return ledger.LessonFact{}, err
	}
	digest := sha256.Sum256(raw)
	fact := ledger.LessonFact{
		AccountID: accountID, SourceFillEventID: outcome.FillEventID, SourceDecisionID: outcome.DecisionID,
		SourceAccountVersion: outcome.accountVersion, SourceOutcomeSHA256: hex.EncodeToString(digest[:]),
		SourceAvailableAt: outcome.AvailableAt, LabelEnd: outcome.ObservedAt, Instrument: outcome.Instrument,
		QuantityRaw: outcome.QuantityRaw, GrossUSD: outcome.GrossUSD, RecordedExitFeeUSD: outcome.FeeUSD,
		CostBasisUSD: outcome.CostBasisUSD, NetPnLUSD: outcome.NetPnLUSD,
	}
	err = fact.Classify()
	return fact, err
}
