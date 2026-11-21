package notifications

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const fakeToken = "123456:fake_token"

func TestTelegramConfirmsPrivateMessage(t *testing.T) {
	telegram := testTelegram(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/bot"+fakeToken+"/sendMessage" {
			t.Errorf("unexpected Telegram method or endpoint")
		}
		var payload struct {
			ChatID    int64  `json:"chat_id"`
			Text      string `json:"text"`
			ParseMode string `json:"parse_mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("invalid request JSON: %v", err)
		}
		if payload.ChatID != 4294967297 || payload.Text != "BUY AAPL <fixture>" || payload.ParseMode != "" {
			t.Errorf("unexpected request payload: %+v", payload)
		}
		writeTelegramSuccess(w)
	}))
	if err := telegram.Send(t.Context(), "BUY AAPL <fixture>"); err != nil {
		t.Fatal(err)
	}
}

func TestTelegramRetriesOnlyExplicitRateLimit(t *testing.T) {
	var attempts atomic.Int32
	telegram := testTelegram(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"ok":false,"error_code":429,"parameters":{"retry_after":1}}`)
			return
		}
		writeTelegramSuccess(w)
	}))
	if err := telegram.Send(t.Context(), "BUY AAPL"); err != nil {
		t.Fatal(err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

func TestTelegramRejectsUnconfirmedMessage(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"not authorized", 401, `{"ok":false,"error_code":401,"description":"123456:fake_token"}`},
		{"server failure", 502, `{"ok":false,"error_code":502}`},
		{"long rate limit", 429, `{"ok":false,"error_code":429,"parameters":{"retry_after":3600}}`},
		{"unconfirmed success", 200, `{"ok":true}`},
		{"wrong chat", 200, `{"ok":true,"result":{"message_id":10,"chat":{"id":14,"type":"private"}}}`},
		{"nonprivate chat", 200, `{"ok":true,"result":{"message_id":10,"chat":{"id":4294967297,"type":"channel"}}}`},
		{"oversized response", 200, strings.Repeat(" ", 64<<10)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var attempts atomic.Int32
			telegram := testTelegram(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			if err := telegram.Send(t.Context(), "BUY AAPL"); err == nil || strings.Contains(err.Error(), fakeToken) {
				t.Fatalf("expected a sanitized failure, got %v", err)
			}
			if got := attempts.Load(); got != 1 {
				t.Fatalf("attempts = %d, want 1", got)
			}
		})
	}
}

func TestTelegramTransportErrorOmitsToken(t *testing.T) {
	telegram, err := NewTelegram(fakeToken, "4294967297")
	if err != nil {
		t.Fatal(err)
	}
	telegram.client.Transport = &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		return nil, fmt.Errorf("dial failed")
	}}
	if err := telegram.Send(t.Context(), "BUY AAPL"); err == nil || strings.Contains(err.Error(), fakeToken) {
		t.Fatalf("expected a sanitized transport failure, got %v", err)
	}
	var updates []analyticsTelegramUpdate
	if err := telegram.commandRequest(t.Context(), "getUpdates", struct{}{}, maxUpdateResponse, &updates); err == nil || strings.Contains(err.Error(), fakeToken) {
		t.Fatalf("expected a sanitized command transport failure, got %v", err)
	}
}

func TestTelegramRequiresNumericPrivateChat(t *testing.T) {
	for _, chatID := range []string{"", "@someone", "0", "-1001234", "9223372036854775808"} {
		if _, err := NewTelegram(fakeToken, chatID); err == nil {
			t.Errorf("accepted invalid chat ID %q", chatID)
		}
	}
}

func testTelegram(t *testing.T, handler http.Handler) *Telegram {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	telegram, err := NewTelegram(fakeToken, "4294967297")
	if err != nil {
		t.Fatal(err)
	}
	telegram.botURL = server.URL + "/bot" + fakeToken
	return telegram
}

func writeTelegramSuccess(w http.ResponseWriter) {
	fmt.Fprint(w, `{"ok":true,"result":{"message_id":10,"chat":{"id":4294967297,"type":"private"}}}`)
}
