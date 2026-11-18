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
	request     Request
	portfolio   contract.Portfolio
	batch       contract.Batch
	completedAt time.Time
}

func ReviewQuotes(req Request, job jobs.Job, receipt jobs.Result, now time.Time) (QuoteReview, error) {
	if !req.validFollow() || job.ID != req.JobID || job.Status != "completed" ||
		job.CreatedAt.IsZero() || now.IsZero() {
		return QuoteReview{}, ErrInvalid
	}
	portfolio, batch, err := loadResearch(job, receipt, req, now)
	if err != nil {
		return QuoteReview{}, err
	}
	return QuoteReview{req, portfolio, batch, receipt.CompletedAt}, nil
}

func (r QuoteReview) IsHold() bool { return allHold(r.batch.Intents) }

func (r QuoteReview) ExpiresAt() time.Time { return r.request.deadline(r.batch.Expires) }

func (r QuoteReview) Authorize(rawQuote []byte, at time.Time) error {
	if err := r.request.SnapshotContext.CheckMarks(at); err != nil {
		return err
	}
	in, err := fromQuote(r.request, r.batch, r.completedAt, rawQuote)
	if err != nil {
		return err
	}
	preview, err := papersim.Preview(in, rawQuote, at)
	if err != nil {
		return err
	}
	return authorize(in, preview, r.request.SnapshotContext, r.portfolio, r.batch)
}
