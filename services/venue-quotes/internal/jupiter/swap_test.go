package jupiter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"tradingbot/venue-quotes/internal/quotes"
)

const testOwner = "BQ72nSv9f3PRyRKCBnHLVrerrv37CYTHm5h3s9VSGQDV"
const testAsset = "So11111111111111111111111111111111111111112"

func TestPrepareSwapExactInputInBothDirections(t *testing.T) {
	var example SwapRequest
	if err := json.Unmarshal(fixture(t, "valid-jupiter-swap-request.json"), &example); err != nil {
		t.Fatal(err)
	}
	var expected PreparedSwap
	if err := json.Unmarshal(fixture(t, "valid-jupiter-prepared-swap.json"), &expected); err != nil {
		t.Fatal(err)
	}
	for _, side := range []string{"BUY", "SELL"} {
		t.Run(side, func(t *testing.T) {
			swap := example
			swap.Side = side
			pair := quotes.Request{Network: quotes.SolanaMainnet, InputMint: nativeUSDC, OutputMint: testAsset, InAmount: swap.InAmount}
			if side == "SELL" {
				pair.InputMint, pair.OutputMint = pair.OutputMint, pair.InputMint
			}
			calls := 0
			client := New("test-key")
			client.http.Transport = transportFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.Method != http.MethodGet || request.URL.Host != "api.jup.ag" || request.URL.Path != "/swap/v2/order" ||
					request.Header.Get("x-api-key") != "test-key" {
					t.Fatalf("unexpected provider request %s %s", request.Method, request.URL)
				}
				want := map[string][]string{
					"inputMint": {pair.InputMint}, "outputMint": {pair.OutputMint}, "amount": {swap.InAmount},
					"taker": {testOwner}, "swapMode": {"ExactIn"}, "slippageBps": {"50"},
					"excludeRouters": {"jupiterz,dflow,okx"}, "maxSupportedTransactionVersion": {"0"},
				}
				if !reflect.DeepEqual(map[string][]string(request.URL.Query()), want) {
					t.Fatalf("unexpected query %v", request.URL.Query())
				}
				return testSwapHTTP(t, fixtureSwapOrder(pair)), nil
			})
			got, err := client.PrepareSwap(context.Background(), testOwner, swap)
			if err != nil || calls != 1 {
				t.Fatalf("prepare calls=%d, error=%v", calls, err)
			}
			if got.RequestedAt.IsZero() || got.ObservedAt.Before(got.RequestedAt) {
				t.Fatalf("invalid observation time: %+v", got)
			}
			want := expected
			want.Side, want.InputMint, want.OutputMint, want.SwapFeeMint = side, pair.InputMint, pair.OutputMint, new(pair.InputMint)
			want.RequestedAt, want.ObservedAt = got.RequestedAt, got.ObservedAt
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("prepared=%+v, expected=%+v", got, want)
			}
		})
	}
}

func TestPrepareSwapRejectsUntrustedOrder(t *testing.T) {
	swap := SwapRequest{Side: "BUY", AssetMint: testAsset, InAmount: "100000001", MinOutAmount: "67500000", SlippageBPS: 50}
	pair := quotes.Request{Network: quotes.SolanaMainnet, InputMint: nativeUSDC, OutputMint: testAsset, InAmount: swap.InAmount}
	for _, test := range []struct {
		name     string
		change   func(*preparedOrder)
		expected error
	}{
		{"below specified minimum", func(o *preparedOrder) { o.MinOut = "67499999" }, ErrMinimumOutput},
		{"error inside status 200", func(o *preparedOrder) { o.ErrorCode = new(1) }, quotes.ErrUnavailable},
		{"unexpected pair", func(o *preparedOrder) { o.OutputMint = nativeUSDC }, quotes.ErrUnavailable},
		{"wrong input amount", func(o *preparedOrder) { o.InAmount = "100000002" }, quotes.ErrUnavailable},
		{"different wallet", func(o *preparedOrder) { o.Taker = new(testAsset) }, quotes.ErrUnavailable},
		{"expired", func(o *preparedOrder) { o.ExpiresAt = new(time.Unix(9999, 0)) }, quotes.ErrUnavailable},
		{"wrong router", func(o *preparedOrder) { o.Router = "jupiterz" }, quotes.ErrUnavailable},
		{"different slippage", func(o *preparedOrder) { o.SlippageBPS = new(uint16(51)) }, quotes.ErrUnavailable},
		{"version unavailable", func(o *preparedOrder) { o.TransactionVersion = nil }, quotes.ErrUnavailable},
		{"unknown version", func(o *preparedOrder) { o.TransactionVersion = new(uint8(1)) }, quotes.ErrUnavailable},
		{"gas sponsored", func(o *preparedOrder) { o.Gasless = new(true) }, quotes.ErrUnavailable},
		{"different payer", func(o *preparedOrder) { o.SignatureFeePayer = new(testAsset) }, quotes.ErrUnavailable},
		{"missing priority fee", func(o *preparedOrder) { o.PriorityFeeLamports = nil }, quotes.ErrUnavailable},
		{"invalid block height", func(o *preparedOrder) { o.LastValidBlockHeight = "0" }, quotes.ErrUnavailable},
		{"missing transaction", func(o *preparedOrder) { o.Transaction = new("") }, quotes.ErrUnavailable},
		{"bad transaction encoding", func(o *preparedOrder) { o.Transaction = new("%%%") }, quotes.ErrUnavailable},
		{"oversize transaction", func(o *preparedOrder) { o.Transaction = new(strings.Repeat("AAAA", 500)) }, quotes.ErrUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			order := fixtureSwapOrder(pair)
			test.change(&order)
			client := New("test-key")
			client.http.Transport = transportFunc(func(_ *http.Request) (*http.Response, error) {
				return testSwapHTTP(t, order), nil
			})
			got, err := client.PrepareSwap(context.Background(), testOwner, swap)
			if !errors.Is(err, test.expected) || got.TransactionBase64 != "" {
				t.Fatalf("expected %v, got preparation %+v / %v", test.expected, got, err)
			}
		})
	}
}

func TestPrepareSwapValidatesInputBeforeProviderCall(t *testing.T) {
	valid := SwapRequest{Side: "SELL", AssetMint: testAsset, InAmount: "1", MinOutAmount: "1", SlippageBPS: 0}
	invalid := []SwapRequest{valid, valid, valid, valid, valid}
	invalid[0].Side = "HOLD"
	invalid[1].AssetMint = nativeUSDC
	invalid[2].InAmount = "18446744073709551616"
	invalid[3].MinOutAmount = "0"
	invalid[4].SlippageBPS = 10_001
	client := New("test-key")
	client.http.Transport = transportFunc(func(_ *http.Request) (*http.Response, error) {
		t.Fatal("invalid request went upstream")
		return nil, nil
	})
	for _, swap := range invalid {
		if _, err := client.PrepareSwap(context.Background(), testOwner, swap); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("invalid swap %+v: %v", swap, err)
		}
	}
	if _, err := client.PrepareSwap(context.Background(), "bad-owner", valid); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("invalid owner: %v", err)
	}
}

func fixtureSwapOrder(pair quotes.Request) preparedOrder {
	order := fixtureOrder(pair)
	order.Taker, order.Transaction = new(testOwner), new("AQIDBA==")
	return preparedOrder{
		orderResponse: order, SlippageBPS: new(uint16(50)), TransactionVersion: new(uint8(0)),
		LastValidBlockHeight: "312345678", Gasless: new(false),
		SignatureFeePayer: new(testOwner), SignatureFeeLamports: new(uint64(5000)),
		PriorityFeePayer: new(testOwner), PriorityFeeLamports: new(uint64(9007199254740993)),
		RentFeeLamports: new(uint64(0)),
	}
}

func testSwapHTTP(t *testing.T, order preparedOrder) *http.Response {
	t.Helper()
	body, err := json.Marshal(order)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body))}
}
