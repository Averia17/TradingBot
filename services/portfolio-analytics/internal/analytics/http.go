package analytics

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

func HTTPHandler(store *Store, accountID string, policy Policy, streamReady func() bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if err := store.Ready(ctx); err != nil || (streamReady != nil && !streamReady()) {
			http.Error(w, "analytics unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/portfolio", func(w http.ResponseWriter, r *http.Request) {
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			http.Error(w, "invalid portfolio report query", http.StatusBadRequest)
			return
		}
		period := "30d"
		if len(query) != 0 {
			if values := query["period"]; len(query) != 1 || len(values) != 1 || !validPeriod(values[0]) {
				http.Error(w, "period must be 7d, 30d or all", http.StatusBadRequest)
				return
			}
			period = query.Get("period")
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		input, err := store.loadReport(ctx, accountID)
		if err != nil {
			reportUnavailable(w, err)
			return
		}
		if streamReady != nil && !streamReady() {
			input.streamUnavailable = true
		}
		report, err := buildReport(input, policy, period)
		if err != nil {
			reportUnavailable(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(w).Encode(report); err != nil {
			slog.Debug("portfolio report response interrupted")
		}
	})
	return mux
}

func reportUnavailable(w http.ResponseWriter, err error) {
	if !errors.Is(err, ErrNoAccount) {
		slog.Warn("portfolio report unavailable", "error_type", errorType(err))
	}
	http.Error(w, "portfolio report unavailable", http.StatusServiceUnavailable)
}

func errorType(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "storage_or_projection"
	}
}
