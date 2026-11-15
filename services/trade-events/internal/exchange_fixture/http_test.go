package exchange_fixture

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"tradingbot/trade-events/internal/fill_event"
	"tradingbot/trade-events/internal/outbox"
)

func TestExchangeFixtureHTTP(t *testing.T) {
	data, err := os.ReadFile("../../../../contracts/valid-trade-event.json")
	if err != nil {
		t.Fatal(err)
	}
	fixture := string(data)
	for _, tc := range []struct {
		name   string
		body   string
		create bool
		err    error
		code   int
		calls  int
	}{
		{"created", fixture, true, nil, http.StatusCreated, 1},
		{"replay", fixture, false, nil, http.StatusOK, 1},
		{"conflicting replay", fixture, false, outbox.ErrConflict, http.StatusConflict, 1},
		{"storage unavailable", fixture, false, errors.New("test dependency error"), http.StatusServiceUnavailable, 1},
		{"live rejected", strings.Replace(fixture, `"mode": "paper"`, `"mode": "live"`, 1), false, nil, http.StatusUnprocessableEntity, 0},
		{"invalid fill rejected", strings.Replace(fixture, `"139.2500"`, `139.25`, 1), false, nil, http.StatusUnprocessableEntity, 0},
		{"oversized body rejected", fixture + strings.Repeat(" ", 4096), false, nil, http.StatusRequestEntityTooLarge, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			handler := NewHandler(func(_ context.Context, event fill_event.Event) (bool, error) {
				calls++
				if event.Source != fill_event.SourceExchangeFixture || event.Mode != "paper" || event.Price != "139.2500" {
					t.Fatalf("invalid test exchange event: %+v", event)
				}
				return tc.create, tc.err
			})
			request := httptest.NewRequest(http.MethodPost, "/v1/test/exchange/fills", strings.NewReader(tc.body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.code || calls != tc.calls {
				t.Fatalf("HTTP %d and %d calls, want HTTP %d and %d calls", response.Code, calls, tc.code, tc.calls)
			}
			if tc.code == http.StatusCreated || tc.code == http.StatusOK {
				var reply struct {
					ID     string `json:"id"`
					Source string `json:"source"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &reply); err != nil || reply.ID != "paper:example:fill-1" || reply.Source != fill_event.SourceExchangeFixture {
					t.Fatalf("unexpected acknowledgement, error %v, reply %+v", err, reply)
				}
			}
		})
	}
}
