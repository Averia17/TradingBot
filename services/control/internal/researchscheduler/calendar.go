package researchscheduler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"tradingbot/control/internal/contract"
)

// PlannedCalendar returns the expected job digest for each non-event window.
// Event-triggered occurrences have separate identities and no calendar journal.
func (s Schedule) PlannedCalendar() ([]contract.ResearchCalendarOccurrence, int) {
	calendar := make([]contract.ResearchCalendarOccurrence, 0, len(s.jobs))
	for _, job := range s.jobs {
		if job.Event == nil {
			calendar = append(calendar, s.calendarOccurrence(job))
		}
	}
	return calendar, len(s.jobs) - len(calendar)
}

func (s Schedule) calendarOccurrence(job occurrence) contract.ResearchCalendarOccurrence {
	jobHash := sha256.Sum256(job.Body)
	return contract.ResearchCalendarOccurrence{
		Version: "0.1", ScheduleID: s.id, ScheduledAt: job.At,
		MaxLatenessSeconds: int(s.window / time.Second), RequestSHA256: hex.EncodeToString(jobHash[:]),
	}
}

func (s Schedule) observeClosedWindow(ctx context.Context, client *http.Client, jobsEndpoint string, job occurrence, sinceClose time.Duration) error {
	logger := slog.With("job_id", job.ID, "scheduled_at", job.At)
	if job.Event != nil || sinceClose > contract.ResearchCalendarObservationLookback {
		logger.Info("research occurrence outside scheduling window")
		return nil
	}
	occurrence := s.calendarOccurrence(job)
	body, err := json.Marshal(occurrence)
	if err != nil {
		return fmt.Errorf("encode calendar observation: %w", err)
	}
	endpoint := strings.TrimSuffix(jobsEndpoint, "/jobs") + "/schedule-occurrences/" + job.ID
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("cannot construct calendar observation request")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return errors.New("calendar observation outcome unknown")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		return fmt.Errorf("Control returned HTTP %d", response.StatusCode)
	}
	const limit = 4 * 1024
	payload, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	var receipt contract.ResearchCalendarReceipt
	if err != nil || len(payload) > limit || contract.Decode(payload, contract.ResearchCalendarOccurrenceReceipt, &receipt) != nil ||
		receipt.JobID != job.ID || receipt.ResearchCalendarOccurrence != occurrence {
		return errors.New("Control receipt did not confirm the calendar observation")
	}
	logger.Info("research calendar window recorded", "status", receipt.Status)
	return nil
}
