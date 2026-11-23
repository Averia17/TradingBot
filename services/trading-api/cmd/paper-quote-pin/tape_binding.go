package main

import (
	"fmt"
	"os"
	"time"

	"tradingbot/trading-api/internal/accounts"
	"tradingbot/trading-api/internal/paper_quotes"
	"tradingbot/trading-api/internal/paper_sim"
	"tradingbot/trading-api/internal/paper_workflow"
)

func replayPublished(root *os.Root, tape *paper_quotes.Tape, pin paper_quotes.QuotePin, review paper_workflow.QuoteReview,
	completedAt, now time.Time) (paper_quotes.Observation, error) {
	if pin.Quote != quoteReference(tape, pin.JobID, pin.Quote.SHA256) {
		return paper_quotes.Observation{}, fmt.Errorf("%w: existing paper quote pin differs from the bound tape", accounts.ErrConflict)
	}
	body, err := readRoot(root, pin.Quote.Ref, paper_sim.MaxInputBytes)
	if err != nil {
		return paper_quotes.Observation{}, fmt.Errorf("read existing pinned quote: %w", err)
	}
	match, err := tape.VerifyPublished(body, completedAt, now, review.ExpiresAt(), review.Authorize)
	if err != nil || match.SHA256 != pin.Quote.SHA256 {
		return paper_quotes.Observation{}, fmt.Errorf("%w: existing paper quote pin differs from the bound tape", accounts.ErrConflict)
	}
	return match, nil
}

func quoteReference(tape *paper_quotes.Tape, jobID, quoteDigest string) paper_quotes.QuoteReference {
	ref := quoteFilename(tape.SHA256(), quoteDigest)
	if tape.AccountID() != "" {
		// Different jobs may select the same observation. Publication cleanup
		// can only remove quote bytes scoped to the same job.
		ref = jobID + "-" + ref
	}
	return paper_quotes.QuoteReference{Ref: ref, SHA256: quoteDigest}
}

func quoteFilename(tapeDigest, quoteDigest string) string {
	return "quote-" + tapeDigest + "-" + quoteDigest + ".json"
}
