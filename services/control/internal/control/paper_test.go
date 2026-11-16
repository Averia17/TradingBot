package control

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tradingbot/control/internal/ledger"
)

const paperID = "00000000-0000-0000-0000-000000000044"

type paperAccountStub struct{}

func (paperAccountStub) Ping(context.Context) error { return nil }

func (paperAccountStub) CreateAccount(_ context.Context, in ledger.CreateAccount) (ledger.Account, bool, error) {
	return ledger.Account{ID: in.ID, Version: 1, CashUSD: in.InitialCashUSD, ReservedCashUSD: "0", Positions: []ledger.Position{}, CreatedAt: time.Unix(1, 0).UTC()}, true, nil
}

func (paperAccountStub) GetAccount(_ context.Context, id string) (ledger.Account, error) {
	if id != paperID {
		return ledger.Account{}, ledger.ErrNotFound
	}
	return ledger.Account{ID: id, Version: 1, CashUSD: "123.45", ReservedCashUSD: "0", Positions: []ledger.Position{}, CreatedAt: time.Unix(1, 0).UTC()}, nil
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
