package control

import (
	"context"
	"errors"
	"net/http"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/jobs"
)

type researchCalendarStore interface {
	RecordCalendarOccurrence(context.Context, contract.ResearchCalendarOccurrence) (contract.ResearchCalendarReceipt, bool, error)
	GetCalendarOccurrence(context.Context, string) (contract.ResearchCalendarReceipt, error)
}

type researchCalendarHTTP struct {
	store researchCalendarStore
}

func (h researchCalendarHTTP) record(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	body, ok := readJSONBody(w, r)
	if !ok {
		return
	}
	var occurrence contract.ResearchCalendarOccurrence
	if contract.Decode(body, contract.ResearchCalendarOccurrenceInput, &occurrence) != nil ||
		occurrence.ScheduledAt.IsZero() || contract.ResearchCalendarJobID(occurrence.ScheduleID, occurrence.ScheduledAt) != id {
		writeError(w, http.StatusUnprocessableEntity, "invalid_calendar_occurrence")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), jobStoreTimeout)
	defer cancel()
	receipt, created, err := h.store.RecordCalendarOccurrence(ctx, occurrence)
	switch {
	case errors.Is(err, jobs.ErrCalendarWindow):
		writeError(w, http.StatusUnprocessableEntity, "outside_calendar_observation_window")
	case errors.Is(err, jobs.ErrConflict), receipt.Status == "job_conflict":
		writeError(w, http.StatusConflict, "calendar_occurrence_conflict")
	case err != nil:
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable")
	case created:
		writeJSON(w, http.StatusCreated, receipt)
	default:
		writeJSON(w, http.StatusOK, receipt)
	}
}

func (h researchCalendarHTTP) get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !jobUUID.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid_job_id")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), jobStoreTimeout)
	defer cancel()
	receipt, err := h.store.GetCalendarOccurrence(ctx, id)
	switch {
	case errors.Is(err, jobs.ErrNotFound):
		writeError(w, http.StatusNotFound, "calendar_occurrence_not_found")
	case err != nil:
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable")
	default:
		writeJSON(w, http.StatusOK, receipt)
	}
}
