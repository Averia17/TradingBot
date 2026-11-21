package quote_http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"tradingbot/venue-quotes/internal/quotes"
)

type quoteFunc func(context.Context, quotes.Request) (quotes.Observation, error)

func (quote quoteFunc) Quote(ctx context.Context, request quotes.Request) (quotes.Observation, error) {
	return quote(ctx, request)
}

func TestQuoteHandlerRejectsInvalidRequestsBeforeProviderCall(t *testing.T) {
	raw, err := os.ReadFile("../../../../contracts/valid-venue-quote-request.json")
	if err != nil {
		t.Fatal(err)
	}
	valid := string(raw)
	for _, test := range []struct {
		name   string
		body   string
		status int
	}{
		{"unknown field", strings.Replace(valid, "in_amount", "taker", 1), http.StatusBadRequest},
		{"duplicate field", strings.Replace(valid, `"network":`, `"network":"solana-mainnet","network":`, 1), http.StatusBadRequest},
		{"wrong network", strings.Replace(valid, "solana-mainnet", "solana-devnet", 1), http.StatusBadRequest},
		{"numeric amount", strings.Replace(valid, `"100000000"`, "100000000", 1), http.StatusBadRequest},
		{"amount overflow", strings.Replace(valid, "100000000", "18446744073709551616", 1), http.StatusBadRequest},
		{"noncanonical amount", strings.Replace(valid, "100000000", "0100000000", 1), http.StatusBadRequest},
		{"second object", valid + "{}", http.StatusBadRequest},
		{"oversized request", valid + strings.Repeat(" ", 1024), http.StatusRequestEntityTooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := Handler(quoteFunc(func(context.Context, quotes.Request) (quotes.Observation, error) {
				t.Fatal("invalid request reached provider")
				return quotes.Observation{}, nil
			}))
			request := httptest.NewRequest(http.MethodPost, "/v1/quotes", strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status=%d, response=%s", response.Code, response.Body)
			}
		})
	}
}

func TestQuoteHandlerReturnsOpaqueProviderError(t *testing.T) {
	raw, err := os.ReadFile("../../../../contracts/valid-venue-quote-request.json")
	if err != nil {
		t.Fatal(err)
	}
	handler := Handler(quoteFunc(func(context.Context, quotes.Request) (quotes.Observation, error) {
		return quotes.Observation{}, quotes.ErrRateLimited
	}))
	request := httptest.NewRequest(http.MethodPost, "/v1/quotes", strings.NewReader(string(raw)))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusTooManyRequests || response.Body.String() != "{\"code\":\"quote_provider_rate_limited\"}\n" {
		t.Fatalf("status=%d, response=%s", response.Code, response.Body)
	}
}

func TestQuoteHandlerBoundsConcurrentProviderCalls(t *testing.T) {
	raw, err := os.ReadFile("../../../../contracts/valid-venue-quote-request.json")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	var requests sync.WaitGroup
	defer func() {
		cancel()
		requests.Wait()
	}()
	entered := make(chan struct{}, maxConcurrentQuotes+1)
	handler := Handler(quoteFunc(func(ctx context.Context, _ quotes.Request) (quotes.Observation, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return quotes.Observation{}, ctx.Err()
	}))
	call := func() *httptest.ResponseRecorder {
		request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/quotes", strings.NewReader(string(raw)))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	for range maxConcurrentQuotes {
		requests.Go(func() { call() })
	}
	for range maxConcurrentQuotes {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("provider calls did not fill the concurrency limit")
		}
	}
	if response := call(); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, response=%s", response.Code, response.Body)
	}
}
