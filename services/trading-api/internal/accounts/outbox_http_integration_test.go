package accounts

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPaperOutboxHTTPRetainsAcknowledgedAccountHistory(t *testing.T) {
	ctx, dsn := ledgerDB(t)
	store := openLedger(t, ctx, dsn)
	createFixture(t, ctx, store, "50")
	request := reserveFixture(1, 1)
	order, _, err := store.Reserve(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	fill := ResolutionRequest{
		AccountID: request.AccountID, OrderID: request.OrderID, EventID: identifier(51),
		Outcome: "filled", GrossUSD: request.GrossUSD, FeeUSD: "0", ObservedAt: order.CreatedAt,
	}
	if _, recorded, err := store.Resolve(ctx, fill); err != nil || !recorded {
		t.Fatalf("record fill: recorded=%t err=%v", recorded, err)
	}

	const token = "test-account-outbox-token"
	handler := NewOutboxHandler(store, token)
	const prefix = "/internal/v1/paper/outbox"
	call := func(method, route, authorization string, status int) []byte {
		t.Helper()
		record := httptest.NewRecorder()
		req := httptest.NewRequest(method, prefix+route, nil).WithContext(ctx)
		req.Header.Set("Authorization", authorization)
		handler.ServeHTTP(record, req)
		if record.Code != status {
			t.Fatalf("%s %s: HTTP %d, want %d", method, route, record.Code, status)
		}
		return record.Body.Bytes()
	}

	const auth = "Bearer " + token
	call("GET", "/fills/next", "", http.StatusUnauthorized)
	data := call("GET", "/fills/next", auth, http.StatusOK)
	var fillEvent struct {
		AccountID string `json:"account_id"`
		EventID   string `json:"event_id"`
		Payload   struct {
			FillEventID string `json:"fill_event_id"`
			GrossUSD    string `json:"gross_usd"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(data, &fillEvent); err != nil || fillEvent.AccountID != request.AccountID ||
		fillEvent.EventID != fill.EventID || fillEvent.Payload.FillEventID != fill.EventID ||
		fillEvent.Payload.GrossUSD != request.GrossUSD {
		t.Fatal("fill response lost its committed identity or exact decimal")
	}
	ackFill := "/fills/" + request.AccountID + "/" + fill.EventID + "/ack"
	call("POST", ackFill, auth, http.StatusNoContent)
	call("POST", ackFill, auth, http.StatusNoContent)
	call("GET", "/fills/next", auth, http.StatusNoContent)

	data = call("GET", "/accounts/next", auth, http.StatusOK)
	var event struct {
		AccountID string `json:"account_id"`
		EventID   string `json:"event_id"`
		Version   int64  `json:"account_version"`
	}
	if err := json.Unmarshal(data, &event); err != nil || event.AccountID != request.AccountID || event.Version != 1 {
		t.Fatal("account event response lost its committed identity or sequence")
	}
	call("POST", "/accounts/"+request.AccountID+"/1/"+identifier(52)+"/ack", auth, http.StatusNotFound)
	ackAccount := "/accounts/" + request.AccountID + "/1/" + event.EventID + "/ack"
	call("POST", ackAccount, auth, http.StatusNoContent)
	call("POST", ackAccount, auth, http.StatusNoContent)
	data = call("GET", "/accounts/"+request.AccountID+"?after=0&through=3", auth, http.StatusOK)
	var history []json.RawMessage
	if err := json.Unmarshal(data, &history); err != nil || len(history) != 3 {
		t.Fatal("account history lost the acknowledged event or later events")
	}
	data = call("GET", "/heads?after=", auth, http.StatusOK)
	var heads []json.RawMessage
	if err := json.Unmarshal(data, &heads); err != nil || len(heads) != 1 {
		t.Fatal("head page did not report the committed account")
	}
	data = call("GET", "/heads?after="+request.AccountID, auth, http.StatusOK)
	if err := json.Unmarshal(data, &heads); err != nil || len(heads) != 0 {
		t.Fatal("head page ignored the keyset cursor")
	}
	data = call("GET", "/heads/"+request.AccountID, auth, http.StatusOK)
	var head struct {
		AccountVersion int64 `json:"account_version"`
	}
	if err := json.Unmarshal(data, &head); err != nil || head.AccountVersion != 3 {
		t.Fatal("account head did not report the current committed version")
	}
}
