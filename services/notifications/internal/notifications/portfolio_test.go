package notifications

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const portfolioFixture = `{
  "schema_version":"1.0","account_id":"paper-test","report_id":"paper-report-25","mode":"paper","currency":"USD","status":"incomplete",
  "account_status":"current","account_version":25,"as_of":"2026-10-19T20:00:00Z",
  "ledger_verified_at":"2026-10-19T20:00:05Z",
  "cash_usd":"100.00000000","reserved_cash_usd":"15.00000000","free_cash_usd":"85.00000000",
  "positions_value_usd":null,"nav_usd":null,"realized_pnl_usd":"3.50000000",
  "unrealized_pnl_usd":null,
  "positions":[
    {"instrument_id":"XNAS:AAPL","registry_version":"test-v1","decimals":8,
     "quantity_raw":"125000000","reserved_quantity_raw":"25000000",
     "basis_usd":"10.00000000","price_usd":null,"mark_observed_at":"2026-10-18T20:00:00Z",
     "market_value_usd":null,"weight_pct":null,"unrealized_pnl_usd":null,"status":"mark_stale"}
  ],
  "performance":{
    "period":"7d","start_at":"2026-10-12T00:00:00Z","end_at":"2026-10-19T00:00:00Z",
    "observations":5,"status":"incomplete","pnl_usd":null,
    "return_pct":null,"benchmark_id":"fixture-index","benchmark_return_pct":"1.2500",
    "excess_pct_points":null,"max_drawdown_pct":null,"operating_status":"coverage_missing"
  }
}`

func TestPortfolioCommandsAuthorizeOwnerAndAcknowledgeUpdates(t *testing.T) {
	var requests atomic.Int32
	analytics := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/v1/portfolio" || r.URL.RawQuery != "period=7d" {
			t.Errorf("unexpected analytics request %s %s", r.Method, r.URL)
		}
		fmt.Fprint(w, portfolioFixture)
	})
	var messages atomic.Int32
	var polls atomic.Int32
	telegram := testTelegram(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bot" + fakeToken + "/getUpdates":
			var payload struct {
				Offset         int64    `json:"offset"`
				Limit          int      `json:"limit"`
				Timeout        int      `json:"timeout"`
				AllowedUpdates []string `json:"allowed_updates"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil ||
				payload.Limit < 1 || payload.Limit > 20 || payload.Timeout < 1 ||
				strings.Join(payload.AllowedUpdates, ",") != "message,callback_query" {
				t.Errorf("unexpected polling payload %+v, %v", payload, err)
			}
			if polls.Add(1) > 1 {
				if payload.Offset != 5 {
					t.Errorf("acknowledgement offset = %d, want 5", payload.Offset)
				}
				fmt.Fprint(w, `{"ok":true,"result":[]}`)
				return
			}
			fmt.Fprint(w, `{"ok":true,"result":[
 {"update_id":1,"message":{"from":{"id":9},"chat":{"id":4294967297,"type":"group"},"text":"/performance 7d"}},
 {"update_id":2,"message":{"from":{"id":8},"chat":{"id":4294967297,"type":"private"},"text":"/performance 7d"}},
 {"update_id":3,"message":{"from":{"id":9},"chat":{"id":42,"type":"private"},"text":"/performance 7d"}},
 {"update_id":4,"message":{"from":{"id":9},"chat":{"id":4294967297,"type":"private"},"text":"/performance 7d"}}
]}`)
		case "/bot" + fakeToken + "/sendMessage":
			messages.Add(1)
			var payload struct {
				Text string `json:"text"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("invalid send payload: %v", err)
			}
			for _, value := range []string{"PAPER", "USD", "2026-10-12", "2026-10-19", "fixture-index", "1.25%", "paper-report-25", "недоступно"} {
				if !strings.Contains(payload.Text, value) {
					t.Errorf("missing %q in %s", value, payload.Text)
				}
			}
			writeTelegramSuccess(w)
		default:
			t.Errorf("unexpected Telegram endpoint %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	commands, err := NewPortfolioCommands(telegram, analytics.URL, "9")
	if err != nil {
		t.Fatal(err)
	}
	var offset int64
	for range 2 {
		if err := commands.poll(t.Context(), &offset); err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != 1 || messages.Load() != 1 {
		t.Fatalf("analytics requests = %d, Telegram replies = %d", requests.Load(), messages.Load())
	}
}

func TestPortfolioUnavailableAndPositionDetails(t *testing.T) {
	replies := make(chan string, 1)
	telegram := testTelegram(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("invalid send payload: %v", err)
		}
		replies <- payload.Text
		writeTelegramSuccess(w)
	}))
	analytics := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, "internal detail")
	})
	commands, err := NewPortfolioCommands(telegram, analytics.URL, "9")
	if err != nil {
		t.Fatal(err)
	}
	if err := commands.handleCommand(t.Context(), "/portfolio"); err != nil {
		t.Fatal(err)
	}
	if reply := <-replies; reply != "Отчёт PAPER сейчас недоступен." {
		t.Fatalf("unexpected outage response %q", reply)
	}
	var report portfolioReport
	if err := json.Unmarshal([]byte(portfolioFixture), &report); err != nil {
		t.Fatal(err)
	}
	text := positionPage(report, 0)
	for _, value := range []string{"PAPER", "XNAS:AAPL", "1.25", "0.25", "10 USD", "2026-10-18", "Цена устарела", "недоступно"} {
		if !strings.Contains(text, value) {
			t.Errorf("missing %q in %q", value, text)
		}
	}
}

func TestPortfolioConfigRejectsUntrustedEndpoint(t *testing.T) {
	telegram, err := NewTelegram(fakeToken, "4294967297")
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"ftp://analytics", "http://id:token@analytics", "http://analytics?q=x", "http://analytics/path", "//analytics"} {
		if _, err := NewPortfolioCommands(telegram, value, "9"); err == nil {
			t.Errorf("accepted invalid analytics origin %q", value)
		}
	}
	if _, err := NewPortfolioCommands(telegram, "http://analytics", "0"); err == nil {
		t.Error("accepted invalid owner user ID")
	}
}

func TestPortfolioButtonsUseOriginalReportAndPaginatePositions(t *testing.T) {
	var report portfolioReport
	if err := json.Unmarshal([]byte(portfolioFixture), &report); err != nil {
		t.Fatal(err)
	}
	report.Performance.Period = "30d"
	position := report.Positions[0]
	report.Positions = nil
	for i := range 6 {
		position.InstrumentID = fmt.Sprintf("fixture-%d", i)
		report.Positions = append(report.Positions, position)
	}
	var requests atomic.Int32
	analytics := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if err := json.NewEncoder(w).Encode(report); err != nil {
			t.Errorf("encode test report: %v", err)
		}
	})
	type reply struct {
		Text     string           `json:"text"`
		Keyboard telegramKeyboard `json:"reply_markup"`
	}
	replies := make(chan reply, 3)
	var acknowledgements atomic.Int32
	telegram := testTelegram(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/answerCallbackQuery") {
			acknowledgements.Add(1)
			fmt.Fprint(w, `{"ok":true,"result":true}`)
			return
		}
		var sent reply
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Errorf("decode sent message: %v", err)
		}
		replies <- sent
		writeTelegramSuccess(w)
	}))
	commands, err := NewPortfolioCommands(telegram, analytics.URL, "9")
	if err != nil {
		t.Fatal(err)
	}
	if err := commands.handleCommand(t.Context(), "/portfolio"); err != nil {
		t.Fatal(err)
	}
	overview := <-replies
	if len(overview.Keyboard.Rows) != 1 || len(overview.Keyboard.Rows[0]) != 2 ||
		!strings.Contains(overview.Text, "paper-report-25") {
		t.Fatalf("unexpected portfolio overview %+v", overview)
	}
	if err := commands.handleCallback(t.Context(), "positions", overview.Keyboard.Rows[0][0].CallbackData); err != nil {
		t.Fatal(err)
	}
	first := <-replies
	if !strings.Contains(first.Text, "Страница 1 из 2") || !strings.Contains(first.Text, "fixture-3") ||
		strings.Contains(first.Text, "fixture-4") || len(first.Keyboard.Rows) < 1 || len(first.Keyboard.Rows[0]) < 1 {
		t.Fatalf("unexpected first positions page %+v", first)
	}
	if err := commands.handleCallback(t.Context(), "next", first.Keyboard.Rows[0][0].CallbackData); err != nil {
		t.Fatal(err)
	}
	next := <-replies
	if !strings.Contains(next.Text, "Страница 2 из 2") || !strings.Contains(next.Text, "fixture-5") ||
		!strings.Contains(next.Text, "paper-report-25") || strings.Contains(next.Text, "fixture-3") {
		t.Fatalf("unexpected second positions page %+v", next)
	}
	if requests.Load() != 1 || acknowledgements.Load() != 2 {
		t.Fatalf("analytics requests = %d, button acknowledgements = %d", requests.Load(), acknowledgements.Load())
	}
}

func TestPortfolioRejectsUnknownSchemaAndCorruptNumbers(t *testing.T) {
	for _, body := range []string{
		strings.Replace(portfolioFixture, `"schema_version":"1.0"`, `"schema_version":"2.0"`, 1),
		strings.Replace(portfolioFixture, `"status":"incomplete"`, `"status":"incomplete","extra_field":true`, 1),
		strings.Replace(portfolioFixture, `"100.00000000"`, `"1e2"`, 1),
		strings.Replace(portfolioFixture, `"paper"`, `"live"`, 1),
	} {
		analytics := testServer(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
		source, err := newPortfolioClient(analytics.URL)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := source.fetch(t.Context(), "7d"); err == nil {
			t.Errorf("accepted malformed analytics response %s", body)
		}
	}
}

func TestPortfolioClientRendersSharedPaperReportFixture(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "contracts", "valid-paper-portfolio-report.json"))
	if err != nil {
		t.Fatal(err)
	}
	source, err := newPortfolioClient("http://analytics")
	if err != nil {
		t.Fatal(err)
	}
	source.client.Transport = portfolioFixtureTransport(body)
	report, err := source.fetch(t.Context(), "7d")
	if err != nil {
		t.Fatal(err)
	}
	summary := portfolioSummary(report)
	positions := positionPage(report, 0)
	performance := portfolioPerformance(report)
	for _, check := range []struct {
		message string
		content string
	}{
		{summary, "1013.5 USD"},
		{summary, "947.5 USD"},
		{positions, "equity:ABC"},
		{positions, "22 USD"},
		{performance, "1.35%"},
		{performance, "test-total-return"},
		{performance, "12.5 USD"},
	} {
		if !strings.Contains(check.message, check.content) {
			t.Errorf("missing %q in %q", check.content, check.message)
		}
	}
}

type portfolioFixtureTransport []byte

func (body portfolioFixtureTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body))}, nil
}

func testServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}
