package hyperliquid

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"tradingbot/venue-quotes/internal/quotes"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (fn transportFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

const metaFixture = `[
  {"universe":[
    {"name":"xyz:TSLA","szDecimals":3,"maxLeverage":10},
    {"name":"xyz:NVDA","szDecimals":3,"maxLeverage":10,"marginTableId":10,"marginMode":"strictIsolated"}
  ],"marginTables":[],"collateralToken":0},
  [
    {"markPx":"440","oraclePx":"440","funding":"0.0005"},
    {"markPx":"181.45","oraclePx":"181.12","funding":"-0.0000021731"}
  ]
]`

const bookFixture = `{"coin":"xyz:NVDA","time":1791216000000,"levels":[
  [{"px":"181.19","sz":"2.555","n":1},{"px":"181.20","sz":"0.123","n":2}],
  [{"px":"181.30","sz":"1","n":1}]
]}`

func TestCapturePinsBothRawResponsesAndMatchesMarketContext(t *testing.T) {
	client := New()
	var calls int
	client.http.Transport = transportFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != http.MethodPost || req.URL.String() != infoURL ||
			req.Header.Get("Content-Type") != "application/json" || req.Header.Get("Authorization") != "" {
			t.Fatalf("unexpected public request %s %s %v", req.Method, req.URL, req.Header)
		}
		var query map[string]string
		if err := json.NewDecoder(req.Body).Decode(&query); err != nil {
			t.Fatal(err)
		}
		body := bookFixture
		if calls == 1 {
			if query["type"] != "metaAndAssetCtxs" || query["dex"] != "xyz" || len(query) != 2 {
				t.Fatalf("unexpected meta request: %v", query)
			}
			body = metaFixture
		} else if query["type"] != "l2Book" || query["coin"] != "xyz:NVDA" || len(query) != 2 {
			t.Fatalf("unexpected book request: %v", query)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	result, err := client.Capture(context.Background(), "NVDA")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || result.Context.MarkPrice != "181.45" || result.Context.OraclePrice != "181.12" ||
		result.Context.Funding != "-0.0000021731" || *result.Market.MarginMode != "strictIsolated" ||
		result.OrderBook.Bids[0].Price != "181.2" || result.OrderBook.Asks[0].Price != "181.3" ||
		result.OrderBook.ProviderTimestamp != 1791216000000 || result.AvailableAt.Before(result.MetaResponse.ReceivedAt) {
		t.Fatalf("unexpected capture: %+v", result)
	}
	for _, part := range []struct {
		response RawObservation
		original string
	}{{result.MetaResponse, metaFixture}, {result.BookResponse, bookFixture}} {
		if string(part.response.Bytes) != part.original ||
			part.response.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(part.original))) {
			t.Fatal("capture did not preserve exact response bytes and hash")
		}
	}
}

func TestCaptureRejectsRateLimitAndWrongMarketBook(t *testing.T) {
	for _, row := range []struct {
		name   string
		status int
		book   string
	}{
		{"wrong coin", http.StatusOK, strings.Replace(bookFixture, "xyz:NVDA", "xyz:TSLA", 1)},
		{"rate limited", http.StatusTooManyRequests, ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			client := New()
			client.http.Transport = transportFunc(func(req *http.Request) (*http.Response, error) {
				var query map[string]string
				_ = json.NewDecoder(req.Body).Decode(&query)
				if query["type"] == "metaAndAssetCtxs" {
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(metaFixture))}, nil
				}
				return &http.Response{StatusCode: row.status, Body: io.NopCloser(strings.NewReader(row.book))}, nil
			})
			_, err := client.Capture(context.Background(), "NVDA")
			if err == nil || (row.status == http.StatusTooManyRequests && !errors.Is(err, quotes.ErrRateLimited)) {
				t.Fatalf("bad response became an observation: %v", err)
			}
		})
	}
}
