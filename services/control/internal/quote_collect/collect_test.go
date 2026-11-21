package quote_collect_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tradingbot/control/internal/paper_quotes"
	"tradingbot/control/internal/quote_collect"
)

var since = time.Date(2026, 11, 18, 14, 31, 0, 0, time.UTC)

func example(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../../../contracts/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func config(t *testing.T) quote_collect.Config {
	t.Helper()
	cfg, err := quote_collect.ParseConfig(example(t, "valid-paper-quote-collection.json"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestCollectNormalizesBothSidesAndExactCash(t *testing.T) {
	fixture := example(t, "valid-paper-quote-fixture-response.json")
	cfg := config(t)
	const rawSaleCash = "120000001"
	sell := cfg.Requests[0]
	sell.Side, sell.AmountRaw = "SELL", "500"
	cfg.Requests = append(cfg.Requests, sell)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/quote" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected fixture request method, path, or content type")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var got map[string]string
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(got) != 4 || got["paper_quote_fixture_schema_version"] != "0.1" {
			t.Errorf("unexpected request fields: %+v", got)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch {
		case got["input_mint"] == cfg.CashMint && got["output_mint"] == sell.TokenMint && got["in_amount"] == "100000000":
			_, _ = w.Write(fixture)
		case got["input_mint"] == sell.TokenMint && got["output_mint"] == cfg.CashMint && got["in_amount"] == "500":
			_, _ = io.WriteString(w, `{"paper_quote_fixture_schema_version":"0.1","input_mint":"TESTx-fixture","output_mint":"USDC-fixture","in_amount":"500","out_amount":"`+rawSaleCash+`","min_out_amount":"119999999","observed_at":"2026-11-18T14:31:01Z","available_at":"2026-11-18T14:31:02Z","expires_at":"2026-11-18T14:31:07Z","fee_bps":20,"fixed_fee_usd":"0.10"}`)
		default:
			t.Errorf("unexpected mint pair or input amount: %+v", got)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	cutoff := since.Add(3 * time.Second)
	collected, err := quote_collect.Collect(t.Context(), server.URL+"/quote", cfg, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	tape, err := paper_quotes.DecodeTape(collected.Raw, collected.SHA256, "", cfg.AccountID)
	if err != nil || tape.IdentityRegistrySHA256() != cfg.IdentityRegistrySHA256 {
		t.Fatalf("tape is not consumable with the configured identity: %v", err)
	}
	for side, gross := range map[string]string{"BUY": "100", "SELL": "120.000001"} {
		observation := tape.Select(since, cutoff, cutoff.Add(time.Second), func(raw []byte, _ time.Time) error {
			if !bytes.Contains(raw, []byte(`"side":"`+side+`"`)) {
				return io.EOF
			}
			return nil
		})
		if observation == nil || observation.Quote.GrossUSD != gross || observation.Quote.QuantityRaw != "500" ||
			observation.Quote.SecurityID != sell.SecurityID || observation.Quote.Instrument != sell.Instrument ||
			!observation.AvailableAt.Equal(since.Add(2*time.Second)) || observation.Quote.Execution != "fill" {
			t.Fatalf("wrong %s observation: %+v", side, observation)
		}
		wantFee := map[string]string{"BUY": "0.30000000", "SELL": "0.34000001"}[side]
		fee, err := observation.Quote.FeeUSD()
		if err != nil || fee != wantFee {
			t.Fatalf("only explicitly modeled %s fees should be charged: %s, %v", side, fee, err)
		}
	}
}

func TestCollectRejectsMalformedMismatchedAndUnavailableReplies(t *testing.T) {
	base := string(example(t, "valid-paper-quote-fixture-response.json"))
	for _, test := range []struct {
		name, from, to string
		status         int
	}{
		{name: "wrong version", from: `"0.1"`, to: `"0.2"`},
		{name: "wrong input mint", from: `"USDC-fixture"`, to: `"wrong-cash"`},
		{name: "wrong output mint", from: `"TESTx-fixture"`, to: `"wrong-wrapper"`},
		{name: "wrong exact input amount", from: `"100000000"`, to: `"100000001"`},
		{name: "zero output", from: `"500"`, to: `"0"`},
		{name: "floating output", from: `"500"`, to: `500.5`},
		{name: "minimum out greater than quoted out", from: `"495"`, to: `"501"`},
		{name: "missing minimum out", from: `"min_out_amount"`, to: `"threshold"`},
		{name: "expired at cutoff", from: `14:31:07Z`, to: `14:31:03Z`},
		{name: "not yet available", from: `14:31:02Z`, to: `14:31:04Z`},
		{name: "reported available before observation", from: `14:31:02Z`, to: `14:31:00Z`},
		{name: "missing fee model", from: `"fee_bps"`, to: `"commission"`},
		{name: "negative fee", from: `"0.10"`, to: `"-0.01"`},
		{name: "duplicate JSON key", from: `"out_amount"`, to: `"in_amount": "100000000", "out_amount"`},
		{name: "unknown transaction field", from: `{`, to: `{"transaction":"unexpected",`},
		{name: "trailing data", from: `}`, to: `} {}`},
		{name: "oversize body", from: `{`, to: strings.Repeat(" ", 320*1024) + "{"},
		{name: "rate limited", status: http.StatusTooManyRequests},
		{name: "source failed", status: http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if test.status != 0 {
					w.WriteHeader(test.status)
					return
				}
				_, _ = io.WriteString(w, strings.Replace(base, test.from, test.to, 1))
			}))
			defer server.Close()
			got, err := quote_collect.Collect(t.Context(), server.URL, config(t), since.Add(3*time.Second))
			if err == nil || got.Raw != nil || got.SHA256 != "" {
				t.Fatalf("invalid response returned usable tape: %+v, %v", got, err)
			}
		})
	}
}

func TestCollectUsesOnlyExplicitLoopbackAndDoesNotFollowRedirects(t *testing.T) {
	cfg := config(t)
	for _, endpoint := range []string{"https://127.0.0.1/quote", "http://localhost/quote", "http://192.0.2.1/quote", "http://user:pass@127.0.0.1/quote"} {
		if _, err := quote_collect.Collect(t.Context(), endpoint, cfg, since); err == nil {
			t.Errorf("accepted endpoint %q", endpoint)
		}
	}
	var requests atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
	}))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	if _, err := quote_collect.Collect(t.Context(), redirect.URL, cfg, since); err == nil || requests.Load() != 0 {
		t.Fatalf("redirect reached destination: %d, %v", requests.Load(), err)
	}
}

func TestCollectRejectsPartialBatchAndHonorsCancellation(t *testing.T) {
	fixture := example(t, "valid-paper-quote-fixture-response.json")
	cfg := config(t)
	cfg.Requests = append(cfg.Requests, cfg.Requests[0])
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			_, _ = w.Write(fixture)
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	got, err := quote_collect.Collect(t.Context(), server.URL, cfg, since.Add(3*time.Second))
	if err == nil || got.Raw != nil || got.SHA256 != "" || calls.Load() != 2 {
		t.Fatalf("partially collected fixtures returned a tape: %+v, calls %d, %v", got, calls.Load(), err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := quote_collect.Collect(ctx, server.URL, cfg, since); err == nil || calls.Load() != 2 {
		t.Fatalf("canceled request reached the fixture server: calls %d, %v", calls.Load(), err)
	}
}

func TestParseConfigRequiresExplicitDecimalsAndValidQuoteScope(t *testing.T) {
	base := string(example(t, "valid-paper-quote-collection.json"))
	for _, change := range []struct{ from, to string }{
		{`, "decimals": 2`, ""},
		{`"decimals": 2`, `"decimals": null`},
		{`"decimals": 2`, `"decimals": 19`},
		{`"USDC-fixture"`, `"TESTx-fixture"`},
		{`"100000000"`, `"9999999999999999999999999999999999999"`},
		{`"BUY"`, `"HOLD"`},
		{`"0.1"`, `"0.2"`},
		{`"fixture-equity-1"`, `""`},
	} {
		if _, err := quote_collect.ParseConfig([]byte(strings.Replace(base, change.from, change.to, 1))); err == nil {
			t.Errorf("accepted quote request replacing %s with %s", change.from, change.to)
		}
	}
	if _, err := quote_collect.ParseConfig([]byte(strings.Replace(base, `"decimals": 2`, `"decimals": 0`, 1))); err != nil {
		t.Fatalf("explicit zero decimals must be supported: %v", err)
	}
}
