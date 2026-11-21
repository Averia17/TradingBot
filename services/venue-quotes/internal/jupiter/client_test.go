package jupiter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"tradingbot/venue-quotes/internal/quote_http"
	"tradingbot/venue-quotes/internal/quotes"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (transport transportFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../../../contracts/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func fixtureOrder(request quotes.Request) orderResponse {
	return orderResponse{
		InputMint: request.InputMint, OutputMint: request.OutputMint, InAmount: request.InAmount,
		OutAmount: "67800123", MinOut: "67500000", SwapMode: "ExactIn",
		FeeBPS: new(uint16(10)), FeeMint: new(request.InputMint), Router: "metis", RequestID: "example-request",
	}
}

func TestHTTPQuoteMapsJupiterExactInputInBothDirections(t *testing.T) {
	raw := fixture(t, "valid-venue-quote-request.json")
	var example quotes.Request
	if err := json.Unmarshal(raw, &example); err != nil {
		t.Fatal(err)
	}
	var expected quotes.Observation
	if err := json.Unmarshal(fixture(t, "valid-venue-quote-observation.json"), &expected); err != nil {
		t.Fatal(err)
	}
	sell := quotes.Request{Network: example.Network, InputMint: example.OutputMint, OutputMint: example.InputMint, InAmount: "67800123"}
	for _, test := range []struct {
		name    string
		request quotes.Request
		apiKey  string
	}{
		{"buy without key", example, ""},
		{"sell with key", sell, "test-key"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := New(test.apiKey)
			calls := 0
			client.http.Transport = transportFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.Method != http.MethodGet || request.URL.Host != "api.jup.ag" ||
					request.URL.Path != "/swap/v2/order" || request.URL.Scheme != "https" ||
					request.Header.Get("x-api-key") != test.apiKey {
					t.Fatalf("unexpected Jupiter request %s %s", request.Method, request.URL)
				}
				query := request.URL.Query()
				if len(query) != 3 || query.Get("inputMint") != test.request.InputMint ||
					query.Get("outputMint") != test.request.OutputMint || query.Get("amount") != test.request.InAmount {
					t.Fatalf("wrong query or extra parameters: %v", query)
				}
				body, err := json.Marshal(fixtureOrder(test.request))
				if err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body))}, nil
			})
			body, err := json.Marshal(test.request)
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/quotes", bytes.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			quote_http.Handler(client).ServeHTTP(response, request)
			var got quotes.Observation
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &got) != nil || calls != 1 {
				t.Fatalf("status=%d calls=%d response=%s", response.Code, calls, response.Body)
			}
			if got.RequestedAt.IsZero() || got.ObservedAt.Before(got.RequestedAt) {
				t.Fatalf("invalid observation time: %+v", got)
			}
			want := expected
			want.RequestedAt, want.ObservedAt = got.RequestedAt, got.ObservedAt
			want.Request, want.SwapFeeMint = test.request, new(test.request.InputMint)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("quote=%+v, expected=%+v", got, want)
			}
		})
	}
}

func TestQuoteRefusesInvalidJupiterResults(t *testing.T) {
	var request quotes.Request
	if err := json.Unmarshal(fixture(t, "valid-venue-quote-request.json"), &request); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*orderResponse)
	}{
		{"unexpected output mint", func(order *orderResponse) { order.OutputMint = request.InputMint }},
		{"wrong input amount", func(order *orderResponse) { order.InAmount = "100000001" }},
		{"exact output", func(order *orderResponse) { order.SwapMode = "ExactOut" }},
		{"missing minimum", func(order *orderResponse) { order.MinOut = "" }},
		{"impossible minimum", func(order *orderResponse) { order.MinOut = "67800124" }},
		{"expired", func(order *orderResponse) { order.ExpiresAt = new(time.Unix(1, 0)) }},
		{"transaction returned", func(order *orderResponse) { order.Transaction = new("unexpected") }},
		{"taker returned", func(order *orderResponse) { order.Taker = new(request.InputMint) }},
		{"provider error", func(order *orderResponse) { order.Error = "no routes" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			order := fixtureOrder(request)
			test.change(&order)
			now := time.Now().UTC()
			if _, err := order.observation(request, now, now); !errors.Is(err, quotes.ErrUnavailable) {
				t.Fatalf("invalid quote accepted: %v", err)
			}
		})
	}
	order := fixtureOrder(request)
	now := time.Now().UTC()
	order.ExpiresAt = new(now.Add(time.Second))
	order.FeeBPS, order.FeeMint = nil, nil
	observation, err := order.observation(request, now, now)
	if err != nil || observation.Firmness != "indicative" || observation.SwapFeeBPS != nil ||
		observation.SwapFeeMint != nil || !order.ExpiresAt.Equal(*observation.ProviderExpiresAt) {
		t.Fatalf("provider expiry or unknown fees lost: %+v, %v", observation, err)
	}
}

func TestQuoteHandlesUpstreamRateLimitRedirectAndResponseSize(t *testing.T) {
	var request quotes.Request
	if err := json.Unmarshal(fixture(t, "valid-venue-quote-request.json"), &request); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		status int
		body   string
		err    error
	}{
		{http.StatusTooManyRequests, "", quotes.ErrRateLimited},
		{http.StatusTemporaryRedirect, "", quotes.ErrUnavailable},
		{http.StatusOK, strings.Repeat(" ", maxResponseBytes+1), quotes.ErrUnavailable},
	} {
		client := New("test-key")
		calls := 0
		client.http.Transport = transportFunc(func(_ *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{
				StatusCode: test.status,
				Header:     http.Header{"Location": {"https://other.invalid/"}},
				Body:       io.NopCloser(strings.NewReader(test.body)),
			}, nil
		})
		if _, err := client.Quote(context.Background(), request); !errors.Is(err, test.err) || calls != 1 {
			t.Fatalf("upstream status %d: error=%v calls=%d", test.status, err, calls)
		}
	}
}
