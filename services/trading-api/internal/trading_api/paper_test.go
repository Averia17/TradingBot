package trading_api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tradingbot/trading-api/internal/accounts"
)

const paperID = "00000000-0000-0000-0000-000000000044"

type paperAccountStub struct{}

type paperReservationProbe struct {
	paperOrderStore
	writes int
}

func (p *paperReservationProbe) Reserve(context.Context, accounts.ReservationRequest) (accounts.Order, bool, error) {
	p.writes++
	return accounts.Order{}, true, nil
}

func (paperAccountStub) Ping(context.Context) error { return nil }

func (paperAccountStub) CreateAccount(_ context.Context, in accounts.CreateAccount) (accounts.Account, bool, error) {
	return accounts.Account{ID: in.ID, Version: 1, CashUSD: in.InitialCashUSD, ReservedCashUSD: "0", Positions: []accounts.Position{}, CreatedAt: time.Unix(1, 0).UTC()}, true, nil
}

func (paperAccountStub) GetAccount(_ context.Context, id string) (accounts.Account, error) {
	if id != paperID {
		return accounts.Account{}, accounts.ErrNotFound
	}
	return accounts.Account{ID: id, Version: 1, CashUSD: "123.45", ReservedCashUSD: "0", Positions: []accounts.Position{}, CreatedAt: time.Unix(1, 0).UTC()}, nil
}

func TestPaperAccountEndpoint(t *testing.T) {
	handler := NewHandlerWithPaper(time.Hour, time.Now, nil, paperAccountStub{})
	capabilities := httptest.NewRecorder()
	handler.ServeHTTP(capabilities, httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil))
	if capabilities.Code != http.StatusOK || !strings.Contains(capabilities.Body.String(), `"paper_accounts":true`) {
		t.Fatalf("paper capability: %d %s", capabilities.Code, capabilities.Body)
	}
	path := "/v1/paper/accounts"
	create := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r := httptest.NewRecorder()
		handler.ServeHTTP(r, req)
		return r
	}
	created := create(`{"account_id":"` + paperID + `","initial_cash_usd":"123.45"}`)
	if created.Code != http.StatusCreated || created.Header().Get("Location") != path+"/"+paperID {
		t.Fatalf("create paper account: %d %s", created.Code, created.Body)
	}
	if got := create(`{"account_id":"` + paperID + `","initial_cash_usd":123.45}`).Code; got != http.StatusBadRequest {
		t.Fatalf("numeric initial funding: got %d", got)
	}
	fetched := httptest.NewRecorder()
	handler.ServeHTTP(fetched, httptest.NewRequest(http.MethodGet, path+"/"+paperID, nil))
	if fetched.Code != http.StatusOK || !strings.Contains(fetched.Body.String(), `"cash_usd":"123.45"`) {
		t.Fatalf("read paper account: %d %s", fetched.Code, fetched.Body)
	}
}

func TestRawPaperReservationCannotAttachResearchProvenance(t *testing.T) {
	probe := &paperReservationProbe{}
	handler := paperOrderHTTP{store: probe}
	body := `{"account_id":"` + paperID + `","order_id":"00000000-0000-0000-0000-000000000101",` +
		`"decision_id":"00000000-0000-0000-0000-000000000102","quote_id":"00000000-0000-0000-0000-000000000103",` +
		`"snapshot_version":1,"instrument":{"instrument_id":"fixture:TSLA","registry_version":"test","decimals":2},` +
		`"side":"BUY","quantity_raw":"10","gross_usd":"10","max_fee_usd":"0","expires_at":"2026-11-17T16:00:00Z"}`
	for _, test := range []struct {
		body string
		want int
	}{
		{strings.TrimSuffix(body, "}") + `,"research_job_id":"00000000-0000-0000-0000-000000000104"}`, http.StatusBadRequest},
		{body, http.StatusCreated},
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/paper/accounts/"+paperID+"/orders", strings.NewReader(test.body))
		req.Header.Set("Content-Type", "application/json")
		req.SetPathValue("id", paperID)
		res := httptest.NewRecorder()
		handler.reserve(res, req)
		if res.Code != test.want {
			t.Fatalf("reserve status %d, want %d", res.Code, test.want)
		}
	}
	if probe.writes != 1 {
		t.Fatalf("forged provenance reached storage: %d writes", probe.writes)
	}
}
