package quote_http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	"tradingbot/venue-quotes/internal/quotes"
)

type Provider interface {
	Quote(context.Context, quotes.Request) (quotes.Observation, error)
}

const maxConcurrentQuotes = 4
const roundTripTimeout = 9 * time.Second

func Handler(provider Provider) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	if provider == nil {
		return mux
	}
	slots := make(chan struct{}, maxConcurrentQuotes)
	handleQuote := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mediaType != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, "expected_json")
			return
		}
		request, err := readRequest(w, r)
		if err != nil {
			var size *http.MaxBytesError
			if errors.As(err, &size) {
				writeError(w, http.StatusRequestEntityTooLarge, "request_too_large")
			} else {
				writeError(w, http.StatusBadRequest, "invalid_quote_request")
			}
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			writeError(w, http.StatusServiceUnavailable, "quote_service_busy")
			return
		}
		if r.Pattern == "POST /v1/quotes/round-trip" {
			ctx, cancel := context.WithTimeout(r.Context(), roundTripTimeout)
			defer cancel()
			observation, err := quotes.RoundTrip(ctx, request, provider.Quote)
			writeProviderResponse(w, observation, err)
			return
		}
		observation, err := provider.Quote(r.Context(), request)
		writeProviderResponse(w, observation, err)
	}
	mux.HandleFunc("POST /v1/quotes", handleQuote)
	mux.HandleFunc("POST /v1/quotes/round-trip", handleQuote)
	return mux
}

func writeProviderResponse(w http.ResponseWriter, observation any, err error) {
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, observation)
	case errors.Is(err, quotes.ErrNoOrderBook):
		writeError(w, http.StatusNotFound, "order_book_unavailable")
	case errors.Is(err, quotes.ErrRateLimited):
		writeError(w, http.StatusTooManyRequests, "quote_provider_rate_limited")
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "quote_provider_timeout")
	default:
		writeError(w, http.StatusBadGateway, "quote_provider_unavailable")
	}
}

func readRequest(w http.ResponseWriter, r *http.Request) (quotes.Request, error) {
	var request quotes.Request
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	opening, err := decoder.Token()
	if err != nil {
		return request, err
	}
	if opening != json.Delim('{') {
		return request, errors.New("expected quote request object")
	}
	fields := map[string]*string{
		"network": &request.Network, "input_mint": &request.InputMint,
		"output_mint": &request.OutputMint, "in_amount": &request.InAmount,
	}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return request, err
		}
		name, _ := key.(string)
		destination := fields[name]
		if destination == nil {
			return request, errors.New("unknown or duplicate quote request field")
		}
		delete(fields, name)
		if err := decoder.Decode(destination); err != nil {
			return request, err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return request, err
	}
	if _, err := decoder.Token(); err == nil {
		return request, errors.New("expected a single JSON request")
	} else if err != io.EOF {
		return request, err
	}
	return request, request.Validate()
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, struct {
		Code string `json:"code"`
	}{Code: code})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
