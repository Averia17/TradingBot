package paperapply

import (
	"time"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/papersim"
)

// QuoteReview validates locally archived quote candidates against the durable
// result and bound snapshot before publication. Follow makes the final
// account-version and risk checks at reservation.
type QuoteReview struct {
	snapshot    SnapshotContext
	portfolio   contract.Portfolio
	batch       contract.Batch
	completedAt time.Time
}

func ReviewQuotes(req Request, job jobs.Job, receipt jobs.Result, now time.Time) (QuoteReview, error) {
	if req.Version != Version || req.Intent != nil || job.ID != req.JobID || job.Status != "completed" ||
		job.CreatedAt.IsZero() || !uuidPattern.MatchString(req.JobID) || now.IsZero() {
		return QuoteReview{}, ErrInvalid
	}
	portfolio, batch, err := loadResearch(job, receipt, req.SnapshotContext, now)
	if err != nil {
		return QuoteReview{}, err
	}
	return QuoteReview{req.SnapshotContext, portfolio, batch, receipt.CompletedAt}, nil
}

func (r QuoteReview) IsHold() bool { return allHold(r.batch.Intents) }

func (r QuoteReview) ExpiresAt() time.Time { return applicationDeadline(r.snapshot, r.batch.Expires) }

func (r QuoteReview) Authorize(rawQuote []byte, at time.Time) error {
	if err := r.snapshot.CheckMarks(at); err != nil {
		return err
	}
	in, err := fromQuote(r.snapshot, r.batch, r.completedAt, rawQuote)
	if err != nil {
		return err
	}
	preview, err := papersim.Preview(in, rawQuote, at)
	if err != nil {
		return err
	}
	return authorize(in, preview, r.snapshot, r.portfolio, r.batch)
}
