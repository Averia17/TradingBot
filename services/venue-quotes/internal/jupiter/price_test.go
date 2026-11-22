package jupiter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"tradingbot/venue-quotes/internal/quotes"
)

const priceMint = "So11111111111111111111111111111111111111112"

func TestPricePreservesExactUSDPriceAndBlock(t *testing.T) {
	client := New("test-key")
	const body = `{"` + priceMint + `":{"usdPrice":1.2345678901234567890123456789012300e-6,"blockId":9007199254740993,"decimals":9,"createdAt":"2024-06-05T08:55:25Z"}}`
	calls := 0
	client.http.Transport = transportFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Method != http.MethodGet || request.URL.Scheme != "https" || request.URL.Host != "api.jup.ag" ||
			request.URL.Path != "/price/v3" || request.URL.RawQuery != "ids="+priceMint || request.Header.Get("x-api-key") != "test-key" {
			t.Fatalf("unexpected Jupiter price request %s %s", request.Method, request.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	price, err := client.Price(context.Background(), priceMint)
	if err != nil || calls != 1 || price.RequestedAt.IsZero() || price.ObservedAt.Before(price.RequestedAt) {
		t.Fatalf("price=%+v calls=%d error=%v", price, calls, err)
	}
	var want Price
	if err := json.Unmarshal(fixture(t, "valid-jupiter-price.json"), &want); err != nil {
		t.Fatal(err)
	}
	want.RequestedAt, want.ObservedAt = price.RequestedAt, price.ObservedAt
	if !reflect.DeepEqual(price, want) {
		t.Fatalf("price=%+v, expected=%+v", price, want)
	}
	encoded, err := json.Marshal(price)
	if err != nil || !strings.Contains(string(encoded), `"block_id":"9007199254740993"`) ||
		!strings.Contains(string(encoded), `"usd_price":"0.00000123456789012345678901234567890123"`) {
		t.Fatalf("price JSON=%s error=%v", encoded, err)
	}
}

func TestPriceRejectsMissingAndInvalidProviderData(t *testing.T) {
	for _, test := range []struct {
		name  string
		entry string
	}{
		{"missing mint", ""},
		{"missing price", `{"blockId":1,"decimals":0}`},
		{"zero price", `{"usdPrice":0e-2,"blockId":1,"decimals":0}`},
		{"negative price", `{"usdPrice":-1,"blockId":1,"decimals":0}`},
		{"quoted price", `{"usdPrice":"1","blockId":1,"decimals":0}`},
		{"unbounded exponent", `{"usdPrice":1e99999999,"blockId":1,"decimals":0}`},
		{"missing decimals", `{"usdPrice":1,"blockId":1}`},
		{"null decimals", `{"usdPrice":1,"blockId":1,"decimals":null}`},
		{"invalid decimals", `{"usdPrice":1,"blockId":1,"decimals":256}`},
		{"missing block", `{"usdPrice":1,"decimals":0}`},
		{"zero block", `{"usdPrice":1,"blockId":0,"decimals":0}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := New("test-key")
			body := "{}"
			if test.entry != "" {
				body = `{"` + priceMint + `":` + test.entry + `}`
			}
			client.http.Transport = transportFunc(func(_ *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			if _, err := client.Price(context.Background(), priceMint); !errors.Is(err, quotes.ErrUnavailable) {
				t.Fatalf("invalid price accepted: %v", err)
			}
		})
	}
}

func TestPriceRejectsInvalidMintBeforeProviderCall(t *testing.T) {
	client := New("test-key")
	client.http.Transport = transportFunc(func(_ *http.Request) (*http.Response, error) {
		t.Fatal("invalid request reached Jupiter")
		return nil, nil
	})
	for _, mint := range []string{"SOL", strings.Repeat("1", 33)} {
		if _, err := client.Price(context.Background(), mint); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("invalid mint accepted: %v", err)
		}
	}
}
