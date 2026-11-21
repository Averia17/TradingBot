package dzengi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"tradingbot/venue-quotes/internal/quote_http"
	"tradingbot/venue-quotes/internal/quotes"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (transport transportFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestOrderBookRouteReadsDzengiWithoutCredentialsAndPreservesDecimals(t *testing.T) {
	client := New()
	client.http.Transport = transportFunc(func(request *http.Request) (*http.Response, error) {
		if want := orderBookURL + "?depth=5&symbol=NVDA"; request.Method != http.MethodGet || request.URL.String() != want || request.Body != nil {
			t.Fatalf("request=%s %s", request.Method, request.URL)
		}
		if len(request.Header) != 1 || request.Header.Get("Accept") != "application/json" {
			t.Fatalf("unexpected request headers %v", request.Header)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{
			"timestamp":1795185000123,"bids":[[99.99,1.0],[100.001,9007199254740993.01]],
			"asks":[[100.50,20],[100.01,1e-8]]
		}`))}, nil
	})
	mux := quote_http.Handler(nil)
	quote_http.RegisterOrderBook(mux, "/v1/order-book", client)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/order-book?symbol=NVDA", nil))
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d, response=%s", response.Code, response.Body)
	}
	var got, want quotes.OrderBook
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.RequestedAt.IsZero() || got.ObservedAt.Before(got.RequestedAt) {
		t.Fatalf("invalid observation time %+v", got)
	}
	raw, err := os.ReadFile("../../../../contracts/valid-venue-order-book.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	want.RequestedAt, want.ObservedAt = got.RequestedAt, got.ObservedAt
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestOrderBookRouteProviderFailures(t *testing.T) {
	for _, test := range []struct {
		name     string
		status   int
		body     string
		err      error
		want     int
		wantCode string
	}{
		{"empty", 200, `{"timestamp":1,"bids":[],"asks":[]}`, nil, 404, "order_book_unavailable"},
		{"missing side", 200, `{"timestamp":1,"bids":[]}`, nil, 502, "quote_provider_unavailable"},
		{"null payload", 200, `null`, nil, 502, "quote_provider_unavailable"},
		{"negative size", 200, `{"timestamp":1,"bids":[[2,-0.1]],"asks":[]}`, nil, 502, "quote_provider_unavailable"},
		{"duplicate price", 200, `{"timestamp":1,"bids":[[1,1],[1.0,2]],"asks":[]}`, nil, 502, "quote_provider_unavailable"},
		{"crossed book", 200, `{"timestamp":1,"bids":[[2,1]],"asks":[[1,1]]}`, nil, 502, "quote_provider_unavailable"},
		{"extra level field", 200, `{"timestamp":1,"bids":[[2,1,1]],"asks":[]}`, nil, 502, "quote_provider_unavailable"},
		{"missing size", 200, `{"timestamp":1,"bids":[[2]],"asks":[]}`, nil, 502, "quote_provider_unavailable"},
		{"large response", 200, `{"timestamp":1,"bids":[],"asks":[]}` + strings.Repeat(" ", maxResponseBytes), nil, 502, "quote_provider_unavailable"},
		{"rate limit", 429, "raw internal detail", nil, 429, "quote_provider_rate_limited"},
		{"rate limit ban", 418, "raw internal detail", nil, 429, "quote_provider_rate_limited"},
		{"upstream down", 503, "raw internal detail", nil, 502, "quote_provider_unavailable"},
		{"timeout", 0, "", context.DeadlineExceeded, 504, "quote_provider_timeout"},
		{"only one side present", 200, `{"timestamp":1,"bids":[],"asks":[[1,2]]}`, nil, 200, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := New()
			client.http.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
				if test.err != nil {
					return nil, test.err
				}
				return &http.Response{
					StatusCode: test.status, Body: io.NopCloser(strings.NewReader(test.body)),
				}, nil
			})
			mux := quote_http.Handler(nil)
			quote_http.RegisterOrderBook(mux, "/v1/order-book", client)
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/order-book?symbol=TSLA&depth=10", nil))
			if response.Code != test.want {
				t.Fatalf("status=%d, response=%s", response.Code, response.Body)
			}
			if test.wantCode != "" && response.Body.String() != `{"code":"`+test.wantCode+`"}`+"\n" {
				t.Fatalf("unexpected error %s", response.Body)
			}
		})
	}
}

func TestOrderBookRejectsInvalidInputBeforeNetwork(t *testing.T) {
	client := New()
	client.http.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("unexpected outgoing request")
		return nil, nil
	})
	mux := quote_http.Handler(nil)
	quote_http.RegisterOrderBook(mux, "/v1/order-book", client)
	for _, query := range []string{
		"", "symbol=tsla", "symbol=TSLA%2FUSD", "symbol=TSLA&symbol=NVDA", "symbol=TSLA&depth=0",
		"symbol=TSLA&depth=6", "symbol=TSLA&depth=05", "symbol=TSLA&depth=5&depth=10",
		"symbol=TSLA&leverage=1", "symbol=TSLA&depth=%ZZ",
	} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/order-book?"+query, nil))
		if response.Code != http.StatusBadRequest {
			t.Errorf("query=%s, status=%d", query, response.Code)
		}
	}
}
