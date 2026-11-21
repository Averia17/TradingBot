package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/paper_quotes"
)

func runAccount(ctx context.Context, research *jobs.Store, root *os.Root, tape *paper_quotes.Tape, poll time.Duration, encoder *json.Encoder) error {
	after := ""
	for {
		page, cancel := context.WithTimeout(ctx, 30*time.Second)
		results, next, err := pinAccountPage(page, research, root, tape, after)
		cancel()
		for _, result := range results {
			// Avoid repeatedly emitting unchanged states in the daemon. A
			// one-shot invocation reports them for operator inspection.
			if poll == 0 || result.Status == "pinned" {
				if err := encoder.Encode(result); err != nil {
					return err
				}
			}
		}
		if poll == 0 {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			slog.Warn("paper quote pin page had refused jobs", "account_id", tape.AccountID(), "error", err)
		}
		after = next
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func pinAccountPage(ctx context.Context, research *jobs.Store, root *os.Root, tape *paper_quotes.Tape, after string) ([]pinResult, string, error) {
	tasks, err := research.ReadyPaperDispatch(ctx, tape.AccountID(), after)
	if err != nil {
		return nil, after, err
	}
	results := make([]pinResult, 0, len(tasks))
	var refused error
	for _, task := range tasks {
		if err := ctx.Err(); err != nil {
			return results, after, errors.Join(refused, err)
		}
		after = task.JobID
		result, err := pinCompleted(ctx, research, root, tape, task.JobID, nil)
		if err != nil {
			refused = errors.Join(refused, fmt.Errorf("paper quote pin job %s: %w", task.JobID, err))
			continue
		}
		results = append(results, result)
	}
	if len(tasks) < jobs.MaxPaperDispatchPage {
		after = ""
	}
	return results, after, refused
}
