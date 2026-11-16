// Package control exposes the local research validation API.
package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"
	"unicode/utf8"

	"tradingbot/control/internal/contract"
)

const (
	maxJSONBody      = 256 * 1024
	concurrentWrites = 4
)

func NewHandler(maxResearchAge time.Duration, now func() time.Time, store jobStore) http.Handler {
	return NewHandlerWithPaper(maxResearchAge, now, store, nil)
}

func NewHandlerWithPaper(maxResearchAge time.Duration, now func() time.Time, store jobStore, paper paperAccountStore) http.Handler {
	mux := http.NewServeMux()
	paperOrders, hasPaperOrders := paper.(paperOrderStore)
	slots := make(chan struct{}, concurrentWrites)
	bounded := func(busyCode string, handle http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			default:
				writeError(w, http.StatusServiceUnavailable, busyCode)
				return
			}
			handle(w, r)
		}
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if store != nil {
			if err := store.Ping(ctx); err != nil {
				writeError(w, http.StatusServiceUnavailable, "storage_unavailable")
				return
			}
		}
		if paper != nil {
			if err := paper.Ping(ctx); err != nil {
				writeError(w, http.StatusServiceUnavailable, "paper_storage_unavailable")
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "storage_enabled": store != nil})
	})
	mux.HandleFunc("GET /v1/capabilities", func(w http.ResponseWriter, _ *http.Request) {
		response := map[string]any{
			"research_batch_schema_version": contract.BatchVersion,
			"research_validation":           true,
			"durable_jobs":                  store != nil,
			"research_job_leases":           store != nil,
			"research_job_call_budget":      store != nil,
			"research_job_results":          store != nil,
			"ledger":                        false,
			"execution":                     false,
		}
		if store != nil {
			response["research_job_schema_version"] = jobSchemaVersion
			response["research_result_schema_version"] = syntheticResultVersion
			response["research_result_schema_versions"] = []string{syntheticResultVersion, offlineResultVersion}
			response["research_result_source_modes"] = []string{syntheticSourceMode, offlineSourceMode}
		}
		if paper != nil {
			response["paper_accounts"] = true
		}
		if hasPaperOrders {
			response["paper_orders"] = true
		}
		writeJSON(w, http.StatusOK, response)
	})
	if paper != nil {
		accounts := paperHTTP{store: paper}
		mux.HandleFunc("POST /v1/paper/accounts", bounded("paper_api_busy", accounts.create))
		mux.HandleFunc("GET /v1/paper/accounts/{id}", bounded("paper_api_busy", accounts.get))
	}
	if hasPaperOrders {
		orders := paperOrderHTTP{store: paperOrders, now: now}
		mux.HandleFunc("POST /v1/paper/accounts/{id}/orders", bounded("paper_api_busy", orders.reserve))
		mux.HandleFunc("GET /v1/paper/accounts/{id}/orders/{order}", bounded("paper_api_busy", orders.get))
		mux.HandleFunc("POST /v1/paper/accounts/{id}/orders/{order}/resolve", bounded("paper_api_busy", orders.resolve))
		mux.HandleFunc("GET /v1/paper/accounts/{id}/events", bounded("paper_api_busy", orders.events))
		mux.HandleFunc("GET /v1/paper/accounts/{id}/outcomes", bounded("paper_api_busy", orders.outcomes))
	}
	if store != nil {
		h := jobHTTP{store: store, now: now, maxResearchAge: maxResearchAge}
		mux.HandleFunc("POST /v1/research/jobs", bounded("job_api_busy", h.create))
		mux.HandleFunc("GET /v1/research/jobs/{id}", h.get)
		mux.HandleFunc("POST /v1/research/jobs/claim", bounded("job_api_busy", h.claim))
		mux.HandleFunc("POST /v1/research/jobs/{id}/heartbeat", bounded("job_api_busy", h.heartbeat))
		mux.HandleFunc("POST /v1/research/jobs/{id}/authorize-call", bounded("job_api_busy", h.authorizeCall))
		mux.HandleFunc("POST /v1/research/jobs/{id}/complete", bounded("job_api_busy", h.complete))
		mux.HandleFunc("GET /v1/research/jobs/{id}/result", h.getResult)
	}
	mux.HandleFunc("POST /v1/research/validate", bounded("validator_busy", func(w http.ResponseWriter, r *http.Request) {
		validateResearch(w, r, now, maxResearchAge)
	}))
	return mux
}

// validateResearch checks the submitted documents as a pair. No submitted
// portfolio data becomes account state, and validation does not create a job.
func validateResearch(w http.ResponseWriter, r *http.Request, now func() time.Time, maxResearchAge time.Duration) {
	body, ok := readJSONBody(w, r)
	if !ok {
		return
	}
	var parts map[string]json.RawMessage
	if contract.Decode(body, nil, &parts) != nil || !hasExactKeys(parts, "batch", "portfolio", "requested_symbols") {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var symbols []string
	if json.Unmarshal(parts["requested_symbols"], &symbols) != nil || contract.CheckSymbols(symbols) != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_requested_symbols")
		return
	}
	portfolio, err := contract.ParsePortfolio(parts["portfolio"])
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_portfolio")
		return
	}
	batch, err := contract.ParseBatch(parts["batch"], now(), maxResearchAge)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_batch")
		return
	}
	if err := contract.CheckAllocation(batch, portfolio, symbols); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_allocation")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"valid": true})
}

func readJSONBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_content_type")
		return nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large")
			return nil, false
		}
		writeError(w, http.StatusBadRequest, "invalid_request")
		return nil, false
	}
	if r.Context().Err() != nil {
		return nil, false
	}
	if !utf8.Valid(body) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return nil, false
	}
	return body, true
}

func hasExactKeys(parts map[string]json.RawMessage, keys ...string) bool {
	if len(parts) != len(keys) {
		return false
	}
	for _, key := range keys {
		if parts[key] == nil {
			return false
		}
	}
	return true
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
