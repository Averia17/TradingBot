package jupiter_http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"time"

	"tradingbot/venue-quotes/internal/jupiter"
	"tradingbot/venue-quotes/internal/quote_http"
	"tradingbot/venue-quotes/internal/quotes"
	"tradingbot/venue-quotes/internal/solana_balance"
)

const (
	requestTimeout   = 11 * time.Second
	maxSwapBodyBytes = 1024
	maxConcurrent    = 4
)

var errInvalidObject = errors.New("invalid request object")

type Market interface {
	quote_http.Provider
	Price(context.Context, string) (jupiter.Price, error)
	PrepareSwap(context.Context, string, jupiter.SwapRequest) (jupiter.PreparedSwap, error)
}

type BalanceProvider interface {
	Balances(context.Context, bool) (solana_balance.Snapshot, error)
}

// Handler serves one configured wallet. An empty prepareOwner disables swap preparation.
func Handler(market Market, balances BalanceProvider, prepareOwner string) *http.ServeMux {
	mux := http.NewServeMux()
	quoteHandler := quote_http.Handler(market)
	mux.Handle("GET /healthz", quoteHandler)
	marketSlots := make(chan struct{}, maxConcurrent)
	balanceSlots := make(chan struct{}, maxConcurrent)
	mux.Handle("POST /v1/quotes", limited(marketSlots, quoteHandler.ServeHTTP))
	mux.Handle("GET /v1/price", limited(marketSlots, priceHandler(market)))
	mux.Handle("GET /v1/balances", limited(balanceSlots, balanceHandler(balances)))
	mux.Handle("POST /v1/swaps/prepare", limited(marketSlots, prepareHandler(market, prepareOwner)))
	return mux
}

func limited(slots chan struct{}, handle http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			writeError(w, http.StatusServiceUnavailable, "adapter_busy")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		handle(w, r.WithContext(ctx))
	}
}

func priceHandler(market Market) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params, err := url.ParseQuery(r.URL.RawQuery)
		mints := params["mint"]
		if err != nil || len(params) != 1 || len(mints) != 1 || !quotes.ValidMint(mints[0]) {
			writeError(w, http.StatusBadRequest, "invalid_price_request")
			return
		}
		price, err := market.Price(r.Context(), mints[0])
		writeProviderResponse(w, price, err, "invalid_price_request")
	}
}

func balanceHandler(balances BalanceProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params, err := url.ParseQuery(r.URL.RawQuery)
		flags := params["fresh"]
		fresh := len(params) == 1 && len(flags) == 1 && flags[0] == "true"
		if err != nil || (len(params) != 0 && !fresh) {
			writeError(w, http.StatusBadRequest, "invalid_balance_request")
			return
		}
		if balances == nil {
			writeError(w, http.StatusServiceUnavailable, "balance_not_configured")
			return
		}
		snapshot, err := balances.Balances(r.Context(), fresh)
		writeProviderResponse(w, snapshot, err, "invalid_balance_request")
	}
}

func prepareHandler(market Market, owner string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if owner == "" {
			writeError(w, http.StatusServiceUnavailable, "swap_preparation_disabled")
			return
		}
		if r.URL.RawQuery != "" {
			writeError(w, http.StatusBadRequest, "invalid_swap_request")
			return
		}
		if mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mediaType != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, "expected_json")
			return
		}
		request, err := readSwapRequest(http.MaxBytesReader(w, r.Body, maxSwapBodyBytes))
		if err != nil {
			writeBodyError(w, err, "invalid_swap_request")
			return
		}
		prepared, err := market.PrepareSwap(r.Context(), owner, request)
		writeProviderResponse(w, prepared, err, "invalid_swap_request")
	}
}

func readSwapRequest(body io.Reader) (jupiter.SwapRequest, error) {
	var request jupiter.SwapRequest
	var side, mint, amount, minimum *string
	var slippage *uint16
	fields := map[string]any{
		"side": &side, "asset_mint": &mint, "in_amount": &amount,
		"min_out_amount": &minimum, "slippage_bps": &slippage,
	}
	if err := decodeObject(body, fields); err != nil {
		return request, err
	}
	if side == nil || mint == nil || amount == nil || minimum == nil || slippage == nil {
		return request, errInvalidObject
	}
	return jupiter.SwapRequest{
		Side: *side, AssetMint: *mint, InAmount: *amount,
		MinOutAmount: *minimum, SlippageBPS: *slippage,
	}, nil
}

func decodeObject(body io.Reader, fields map[string]any) error {
	decoder := json.NewDecoder(body)
	opening, err := decoder.Token()
	if err != nil {
		return err
	}
	if opening != json.Delim('{') {
		return errInvalidObject
	}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return err
		}
		name, _ := key.(string)
		target, known := fields[name]
		if !known {
			return errInvalidObject
		}
		delete(fields, name)
		if err := decoder.Decode(target); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	if _, err := decoder.Token(); err == io.EOF {
		return nil
	} else if err != nil {
		return err
	}
	return errInvalidObject
}

func writeBodyError(w http.ResponseWriter, err error, invalidCode string) {
	var size *http.MaxBytesError
	if errors.As(err, &size) {
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large")
		return
	}
	writeError(w, http.StatusBadRequest, invalidCode)
}

func writeProviderResponse(w http.ResponseWriter, result any, err error, invalidCode string) {
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, result)
	case errors.Is(err, jupiter.ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, invalidCode)
	case errors.Is(err, jupiter.ErrMinimumOutput):
		writeError(w, http.StatusUnprocessableEntity, "minimum_output_not_met")
	case errors.Is(err, quotes.ErrRateLimited):
		writeError(w, http.StatusTooManyRequests, "provider_rate_limited")
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "provider_timeout")
	default:
		writeError(w, http.StatusBadGateway, "provider_unavailable")
	}
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, struct {
		Code string `json:"code"`
	}{Code: code})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
