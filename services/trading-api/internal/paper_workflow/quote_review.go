package paper_workflow

import (
	"time"

	"tradingbot/trading-api/internal/jobs"
)

// QuoteReview validates locally archived quote candidates against the durable
// result and bound snapshot before publication. Follow makes the final
// account-version and risk checks at reservation.
type QuoteReview struct {
	application applicationPlan
}

func ReviewQuotes(req Request, job jobs.Job, receipt jobs.Result, now time.Time) (QuoteReview, error) {
	if !req.validFollow() || job.ID != req.JobID || job.Status != "completed" ||
		job.CreatedAt.IsZero() || now.IsZero() {
		return QuoteReview{}, ErrInvalid
	}
	application, err := validateCompletedResearch(job, receipt, req, now)
	if err != nil {
		return QuoteReview{}, err
	}
	return QuoteReview{application}, nil
}

func (r QuoteReview) IsHold() bool { return r.application.isHold() }

func (r QuoteReview) ExpiresAt() time.Time { return r.application.deadline() }

func (r QuoteReview) Authorize(rawQuote []byte, at time.Time) error {
	if err := r.application.request.SnapshotContext.CheckMarks(at); err != nil {
		return err
	}
	_, err := r.application.authorizedOrder(rawQuote, at)
	return err
}
