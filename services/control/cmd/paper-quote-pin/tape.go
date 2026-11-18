package main

import (
	"fmt"
	"os"
	"time"

	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paperapply"
	"tradingbot/control/internal/paperquotes"
	"tradingbot/control/internal/papersim"
)

func replayPublished(root *os.Root, tape *paperquotes.Tape, pin quotePin, review paperapply.QuoteReview,
	completedAt, now time.Time) (paperquotes.Observation, error) {
	if pin.Quote != quoteReference(tape, pin.JobID, pin.Quote.SHA256) {
		return paperquotes.Observation{}, fmt.Errorf("%w: existing paper quote pin differs from the bound tape", ledger.ErrConflict)
	}
	body, err := readRoot(root, pin.Quote.Ref, papersim.MaxInputBytes)
	if err != nil {
		return paperquotes.Observation{}, fmt.Errorf("read existing pinned quote: %w", err)
	}
	match, err := tape.VerifyPublished(body, completedAt, now, review.ExpiresAt(), review.Authorize)
	if err != nil || match.SHA256 != pin.Quote.SHA256 {
		return paperquotes.Observation{}, fmt.Errorf("%w: existing paper quote pin differs from the bound tape", ledger.ErrConflict)
	}
	return match, nil
}

func quoteReference(tape *paperquotes.Tape, jobID, quoteDigest string) pinQuote {
	ref := quoteFilename(tape.SHA256(), quoteDigest)
	if tape.AccountID() != "" {
		// Different jobs may select the same observation. Publication cleanup
		// can only remove quote bytes scoped to the same job.
		ref = jobID + "-" + ref
	}
	return pinQuote{Ref: ref, SHA256: quoteDigest}
}

func quoteFilename(tapeDigest, quoteDigest string) string {
	return "quote-" + tapeDigest + "-" + quoteDigest + ".json"
}
