package exchange_fixture

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"time"

	"tradingbot/trade-events/internal/fill_event"
	"tradingbot/trade-events/internal/outbox"
)

type recordFill func(context.Context, fill_event.Event) (bool, error)

func NewHandler(record recordFill) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("POST /v1/test/exchange/fills", func(w http.ResponseWriter, r *http.Request) {
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			http.Error(w, "application/json is required", http.StatusUnsupportedMediaType)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4*1024))
		var limitErr *http.MaxBytesError
		if errors.As(err, &limitErr) {
			http.Error(w, "fill is too large", http.StatusRequestEntityTooLarge)
			return
		}
		if err != nil {
			http.Error(w, "could not read fill", http.StatusBadRequest)
			return
		}
		event, err := fill_event.Decode(body)
		if err != nil || event.Mode != "paper" {
			http.Error(w, "expected a valid paper-mode fill", http.StatusUnprocessableEntity)
			return
		}
		event.Source = fill_event.SourceExchangeFixture

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		created, err := record(ctx, event)
		if errors.Is(err, outbox.ErrConflict) {
			http.Error(w, "fill ID already exists with different contents", http.StatusConflict)
			return
		}
		if err != nil {
			slog.Error("exchange fixture storage failed", "event_id", event.ID)
			http.Error(w, "could not confirm that the fill was stored", http.StatusServiceUnavailable)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		slog.Info("exchange fixture committed", "event_id", event.ID, "created", created)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(struct {
			ID     string `json:"id"`
			Source string `json:"source"`
		}{ID: event.ID, Source: event.Source})
	})
	return mux
}
