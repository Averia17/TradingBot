package jupiter_http

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"tradingbot/venue-quotes/internal/jupiter"
	"tradingbot/venue-quotes/internal/quotes"
	"tradingbot/venue-quotes/internal/solana_balance"
)

const (
	testOwner  = "11111111111111111111111111111111"
	testMint   = "So11111111111111111111111111111111111111112"
	testUSDC   = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	swapBody   = `{"side":"BUY","asset_mint":"` + testMint + `","in_amount":"1000000","min_out_amount":"10","slippage_bps":0}`
	quoteBody  = `{"network":"solana-mainnet","input_mint":"` + testUSDC + `","output_mint":"` + testMint + `","in_amount":"1000000"}`
	secretText = "provider request included credentials"
)

type testMarket struct {
	quote   func(context.Context, quotes.Request) (quotes.Observation, error)
	price   func(context.Context, string) (jupiter.Price, error)
	prepare func(context.Context, string, jupiter.SwapRequest) (jupiter.PreparedSwap, error)
}

func (m testMarket) Quote(ctx context.Context, request quotes.Request) (quotes.Observation, error) {
	return m.quote(ctx, request)
}

func (m testMarket) Price(ctx context.Context, mint string) (jupiter.Price, error) {
	return m.price(ctx, mint)
}

func (m testMarket) PrepareSwap(ctx context.Context, owner string, request jupiter.SwapRequest) (jupiter.PreparedSwap, error) {
	return m.prepare(ctx, owner, request)
}

type testBalances func(context.Context, bool) (solana_balance.Snapshot, error)

func (b testBalances) Balances(ctx context.Context, fresh bool) (solana_balance.Snapshot, error) {
	return b(ctx, fresh)
}

func makeRequest(t *testing.T, handler http.Handler, method, path, body, contentType string, status int, expected string) {
	t.Helper()
	request := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != status || !strings.Contains(response.Body.String(), expected) {
		t.Fatalf("%s %s: status=%d body=%s", method, path, response.Code, response.Body)
	}
	if strings.Contains(response.Body.String(), secretText) {
		t.Fatal("provider error leaked in response")
	}
	if strings.HasPrefix(path, "/v1/") && status != http.StatusNotFound && response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("missing no-store for %s %s", method, path)
	}
}

func TestAdapterRoutes(t *testing.T) {
	market := testMarket{
		quote: func(_ context.Context, request quotes.Request) (quotes.Observation, error) {
			return quotes.Observation{Request: request, Provider: "jupiter"}, nil
		},
		price: func(ctx context.Context, mint string) (jupiter.Price, error) {
			if _, bounded := ctx.Deadline(); mint != testMint || !bounded {
				t.Fatalf("price mint=%q bounded=%t", mint, bounded)
			}
			return jupiter.Price{Mint: mint, USDPrice: "0.1234567890123456789"}, nil
		},
		prepare: func(_ context.Context, owner string, request jupiter.SwapRequest) (jupiter.PreparedSwap, error) {
			if owner != testOwner || request != (jupiter.SwapRequest{Side: "BUY", AssetMint: testMint, InAmount: "1000000", MinOutAmount: "10"}) {
				t.Fatalf("prepare owner=%q request=%+v", owner, request)
			}
			return jupiter.PreparedSwap{}, nil
		},
	}
	var refreshes []bool
	balances := testBalances(func(_ context.Context, fresh bool) (solana_balance.Snapshot, error) {
		refreshes = append(refreshes, fresh)
		return solana_balance.Snapshot{Owner: testOwner}, nil
	})
	handler := Handler(market, balances, testOwner)
	makeRequest(t, handler, http.MethodGet, "/healthz", "", "", http.StatusNoContent, "")
	makeRequest(t, handler, http.MethodPost, "/v1/quotes", quoteBody, "application/json", http.StatusOK, `"provider":"jupiter"`)
	makeRequest(t, handler, http.MethodGet, "/v1/price?mint="+testMint, "", "", http.StatusOK, `"usd_price":"0.1234567890123456789"`)
	makeRequest(t, handler, http.MethodGet, "/v1/balances", "", "", http.StatusOK, testOwner)
	makeRequest(t, handler, http.MethodGet, "/v1/balances?fresh=true", "", "", http.StatusOK, testOwner)
	if len(refreshes) != 2 || refreshes[0] || !refreshes[1] {
		t.Fatalf("fresh flags=%v", refreshes)
	}
	makeRequest(t, handler, http.MethodPost, "/v1/swaps/prepare", swapBody, "application/json", http.StatusOK, "{")
	makeRequest(t, handler, http.MethodPost, "/v1/swaps/execute", "{}", "application/json", http.StatusNotFound, "")
}

func TestDisabledWalletRoutes(t *testing.T) {
	handler := Handler(testMarket{}, nil, "")
	makeRequest(t, handler, http.MethodGet, "/v1/balances", "", "", http.StatusServiceUnavailable, "balance_not_configured")
	makeRequest(t, handler, http.MethodPost, "/v1/swaps/prepare", swapBody, "application/json", http.StatusServiceUnavailable, "swap_preparation_disabled")
}

func TestRejectsInvalidInputsBeforeProviderCall(t *testing.T) {
	handler := Handler(testMarket{}, testBalances(nil), testOwner)
	for _, path := range []string{
		"/v1/price", "/v1/price?mint=", "/v1/price?mint=A", "/v1/price?mint=" + testMint + "&mint=" + testMint,
		"/v1/price?mint=" + testMint + "&rpc=other", "/v1/balances?fresh=false", "/v1/balances?fresh=true&fresh=true",
		"/v1/balances?owner=" + testOwner,
	} {
		makeRequest(t, handler, http.MethodGet, path, "", "", http.StatusBadRequest, "invalid_")
	}
	for _, body := range []string{
		strings.Replace(swapBody, `"side":"BUY"`, `"side":"BUY","taker":"override"`, 1),
		strings.Replace(swapBody, `"side":"BUY"`, `"side":"BUY","side":"SELL"`, 1),
		strings.Replace(swapBody, `"side":"BUY"`, `"side":null`, 1),
		strings.Replace(swapBody, `"in_amount":"1000000"`, `"in_amount":1000000`, 1),
		strings.Replace(swapBody, `,"slippage_bps":0`, "", 1),
		strings.Replace(swapBody, `"slippage_bps":0`, `"slippage_bps":null`, 1),
		strings.Replace(swapBody, `"slippage_bps":0`, `"slippage_bps":0.5`, 1),
		swapBody + "{}",
	} {
		makeRequest(t, handler, http.MethodPost, "/v1/swaps/prepare", body, "application/json", http.StatusBadRequest, "invalid_swap_request")
	}
	makeRequest(t, handler, http.MethodPost, "/v1/swaps/prepare?owner="+testOwner, swapBody, "application/json", http.StatusBadRequest, "invalid_swap_request")
	makeRequest(t, handler, http.MethodPost, "/v1/swaps/prepare", swapBody, "text/plain", http.StatusUnsupportedMediaType, "expected_json")
	makeRequest(t, handler, http.MethodPost, "/v1/swaps/prepare", swapBody+strings.Repeat(" ", 1024), "application/json", http.StatusRequestEntityTooLarge, "request_too_large")
}

func TestProviderErrorsStayOpaque(t *testing.T) {
	market := testMarket{
		price: func(context.Context, string) (jupiter.Price, error) {
			return jupiter.Price{}, quotes.ErrRateLimited
		},
		prepare: func(context.Context, string, jupiter.SwapRequest) (jupiter.PreparedSwap, error) {
			return jupiter.PreparedSwap{}, errors.Join(jupiter.ErrInvalidRequest, errors.New(secretText))
		},
	}
	handler := Handler(market, testBalances(func(context.Context, bool) (solana_balance.Snapshot, error) {
		return solana_balance.Snapshot{}, errors.New(secretText)
	}), testOwner)
	makeRequest(t, handler, http.MethodGet, "/v1/price?mint="+testMint, "", "", http.StatusTooManyRequests, "provider_rate_limited")
	makeRequest(t, handler, http.MethodPost, "/v1/swaps/prepare", swapBody, "application/json", http.StatusBadRequest, "invalid_swap_request")
	makeRequest(t, handler, http.MethodGet, "/v1/balances", "", "", http.StatusBadGateway, "provider_unavailable")

	market.price = func(context.Context, string) (jupiter.Price, error) {
		return jupiter.Price{}, context.DeadlineExceeded
	}
	makeRequest(t, Handler(market, nil, ""), http.MethodGet, "/v1/price?mint="+testMint, "", "", http.StatusGatewayTimeout, "provider_timeout")

	market.prepare = func(context.Context, string, jupiter.SwapRequest) (jupiter.PreparedSwap, error) {
		return jupiter.PreparedSwap{}, jupiter.ErrMinimumOutput
	}
	makeRequest(t, Handler(market, nil, testOwner), http.MethodPost, "/v1/swaps/prepare", swapBody, "application/json", http.StatusUnprocessableEntity, "minimum_output_not_met")
}

func TestMarketCapacityLeavesWalletReadsAvailable(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	var requests sync.WaitGroup
	defer func() {
		cancel()
		requests.Wait()
	}()
	entered := make(chan struct{}, maxConcurrent)
	market := testMarket{
		price: func(ctx context.Context, _ string) (jupiter.Price, error) {
			entered <- struct{}{}
			<-ctx.Done()
			return jupiter.Price{}, ctx.Err()
		},
	}
	handler := Handler(market, testBalances(func(context.Context, bool) (solana_balance.Snapshot, error) {
		return solana_balance.Snapshot{Owner: testOwner}, nil
	}), testOwner)
	for range maxConcurrent {
		requests.Go(func() {
			request := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/price?mint="+testMint, nil)
			handler.ServeHTTP(httptest.NewRecorder(), request)
		})
	}
	for range maxConcurrent {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("price calls did not fill market capacity")
		}
	}
	makeRequest(t, handler, http.MethodPost, "/v1/quotes", quoteBody, "application/json", http.StatusServiceUnavailable, "adapter_busy")
	makeRequest(t, handler, http.MethodPost, "/v1/swaps/prepare", swapBody, "application/json", http.StatusServiceUnavailable, "adapter_busy")
	makeRequest(t, handler, http.MethodGet, "/v1/balances", "", "", http.StatusOK, testOwner)
}
