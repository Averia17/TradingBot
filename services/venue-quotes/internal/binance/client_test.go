package binance

import (
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

func TestOrderBookRouteUsesPublicBinanceSnapshot(t *testing.T) {
	client := New()
	client.http.Transport = transportFunc(func(req *http.Request) (*http.Response, error) {
		wantURL := orderBookURL + "?limit=50&symbol=NVDABUSDT"
		if req.Method != http.MethodGet || req.URL.String() != wantURL || req.Body != nil {
			t.Fatalf("unexpected request %s %s", req.Method, req.URL)
		}
		if len(req.Header) != 1 || req.Header.Get("Accept") != "application/json" {
			t.Fatal("unexpected outgoing headers")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{
			"lastUpdateId":280686768,"bids":[["129.78000000","29.36900000"]],
			"asks":[["129.79000000","1.08300000"]]
		}`))}, nil
	})
	mux := quote_http.Handler(nil)
	quote_http.RegisterOrderBook(mux, "/v1/order-book/binance", client)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/order-book/binance?symbol=NVDABUSDT&depth=50", nil))
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d, response=%s", response.Code, response.Body)
	}
	var got, want quotes.OrderBook
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.RequestedAt.IsZero() || got.ObservedAt.Before(got.RequestedAt) || strings.Contains(response.Body.String(), `"provider_timestamp"`) {
		t.Fatalf("invalid snapshot metadata %s", response.Body)
	}
	raw, err := os.ReadFile("../../../../contracts/valid-venue-order-book-binance.json")
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

func TestOrderBookRouteRejectsInvalidBinanceResponses(t *testing.T) {
	for _, row := range []struct {
		name       string
		status     int
		body       string
		wantStatus int
	}{
		{"unknown symbol", 400, `{"code":-1121,"msg":"Invalid symbol."}`, 404},
		{"rejected request", 400, `{"code":-1102}`, 502},
		{"missing sequence", 200, `{"bids":[["130","1"]],"asks":[]}`, 502},
		{"missing side", 200, `{"lastUpdateId":1,"bids":[]}`, 502},
		{"double empty book", 200, `{"lastUpdateId":1,"bids":[],"asks":[]}`, 404},
		{"rate limited", 429, "", 429},
	} {
		t.Run(row.name, func(t *testing.T) {
			client := New()
			client.http.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: row.status, Body: io.NopCloser(strings.NewReader(row.body))}, nil
			})
			mux := quote_http.Handler(nil)
			quote_http.RegisterOrderBook(mux, "/v1/order-book/binance", client)
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/order-book/binance?symbol=EPAMBUSDT", nil))
			if response.Code != row.wantStatus {
				t.Fatalf("status=%d, response=%s", response.Code, response.Body)
			}
		})
	}
}
