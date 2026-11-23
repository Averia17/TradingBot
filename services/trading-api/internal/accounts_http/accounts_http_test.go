package accounts_http

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"tradingbot/trading-api/internal/accounts"
)

const testToken = "fixture-token-1234567890-paper-internal"

func TestPrivateAccountEndpointsRequireInternalToken(t *testing.T) {
	handler := NewHandler(nil, testToken, nil)
	for _, path := range []string{"/internal/v1/paper/applications/fixture", "/v1/paper/accounts/fixture"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous request for %s returned %d", path, response.Code)
		}
	}
}

func TestAccountHTTPReservationAndResolutionReplay(t *testing.T) {
	ctx, store := accountHTTPDatabase(t)
	server := httptest.NewServer(NewHandler(store, testToken, nil))
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, testToken)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	accountID := "e36eed0a-2a01-44ce-bd46-6f75c5352f7c"
	jobID := "0c617492-5001-4fed-a3fd-5b2f21907fb8"
	if err := client.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	_, created, err := client.CreateAccount(ctx, accounts.CreateAccount{ID: accountID, InitialCashUSD: "0.99999999"})
	if err != nil || !created {
		t.Fatalf("create: created=%t error=%v", created, err)
	}
	expiry := time.Now().UTC().Add(time.Minute)
	input := accounts.ApplicationInput{
		AccountID: accountID, JobID: jobID, SnapshotVersion: 1,
		RequestSHA256: sha256.Sum256([]byte("request")),
		Quote:         []byte("{ \n\"fixture\":true}"), ReserveBefore: expiry,
	}
	order := accounts.ReservationRequest{
		AccountID: accountID, ResearchJobID: jobID, SnapshotVersion: 1,
		OrderID: "6b232c2e-7fb2-41d4-9966-6e60c50540bd", DecisionID: "3183982f-8d8a-4f04-a25f-db951ae7ad89",
		QuoteID: "dea59b27-cf53-46c7-adb5-fc2f104aee9e", Side: "BUY",
		Instrument:  accounts.Instrument{ID: "fixture:ABC", RegistryVersion: "v1", Decimals: 0},
		QuantityRaw: "1", GrossUSD: "0.12345678", MaxFeeUSD: "0", ExpiresAt: expiry,
	}
	reserved, created, err := client.ReserveApplication(ctx, order, input)
	if err != nil || !created || reserved.Status != "reserved" {
		t.Fatalf("reserve: status=%s created=%t error=%v", reserved.Status, created, err)
	}
	fixed, err := client.ApplicationAfterReservations(ctx, accountID, jobID)
	if err != nil || fixed.RequestSHA256 != input.RequestSHA256 || !bytes.Equal(fixed.Quote, input.Quote) {
		t.Fatalf("bound application changed exact payload: %+v %v", fixed, err)
	}
	if _, _, err := client.ReserveApplication(ctx, order, accounts.ApplicationInput{}); !errors.Is(err, accounts.ErrInvalid) {
		t.Fatalf("invalid reservation lost its domain error: %v", err)
	}
	outcome := accounts.ResolutionRequest{
		AccountID: accountID, OrderID: order.OrderID, EventID: "48d78608-8c4e-4b88-9f46-8413777faad4", Outcome: "filled",
		QuantityRaw: "1", GrossUSD: order.GrossUSD, FeeUSD: "0", ObservedAt: reserved.CreatedAt,
	}
	if _, recorded, err := client.Resolve(ctx, outcome); err != nil || !recorded {
		t.Fatalf("recorded=%t error=%v", recorded, err)
	}
	if _, recorded, err := client.Resolve(ctx, outcome); err != nil || recorded {
		t.Fatalf("repeat recorded=%t error=%v", recorded, err)
	}
	postFill, err := client.GetAccount(ctx, accountID)
	if err != nil || postFill.Version != 3 || postFill.CashUSD != "0.87654321" || postFill.ReservedCashUSD != "0" {
		t.Fatalf("account after fill: %+v %v", postFill, err)
	}
	if _, err := client.RiskActivity(ctx, accountID, 1); !errors.Is(err, accounts.ErrStale) {
		t.Fatalf("stale version: %v", err)
	}
	result := []byte("{\n \"status\":\"applied\"\n}")
	record, err := client.RecordApplication(ctx, input, order.OrderID, result, expiry)
	if err != nil || !bytes.Equal(result, record.Result) {
		t.Fatalf("record result changed exact bytes: %+v %v", record, err)
	}
}

func TestAccountHTTPReconciliationVerifiesOriginalBytes(t *testing.T) {
	ctx, store := accountHTTPDatabase(t)
	server := httptest.NewServer(NewHandler(store, testToken, nil))
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, testToken)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	const accountID = "e36eed0a-2a01-44ce-bd46-6f75c5352f7c"
	_, _, err = client.CreateAccount(ctx, accounts.CreateAccount{ID: accountID, InitialCashUSD: "10"})
	if err != nil {
		t.Fatal(err)
	}
	order, _, err := client.Reserve(ctx, accounts.ReservationRequest{
		AccountID: accountID, SnapshotVersion: 1,
		OrderID: "6b232c2e-7fb2-41d4-9966-6e60c50540bd", DecisionID: "3183982f-8d8a-4f04-a25f-db951ae7ad89",
		QuoteID: "dea59b27-cf53-46c7-adb5-fc2f104aee9e", Side: "BUY",
		Instrument:  accounts.Instrument{ID: "fixture:ABC", RegistryVersion: "v1"},
		QuantityRaw: "1", GrossUSD: "0.12345678", MaxFeeUSD: "0", ExpiresAt: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = client.Resolve(ctx, accounts.ResolutionRequest{
		AccountID: accountID, OrderID: order.OrderID,
		EventID: "48d78608-8c4e-4b88-9f46-8413777faad4", Outcome: "unknown",
	})
	if err != nil {
		t.Fatal(err)
	}
	observed := accounts.ReconciliationObservation{
		Version: accounts.ReconciliationVersion, Source: accounts.ReconciliationSource,
		AccountID: accountID, OrderID: order.OrderID, QuoteID: order.QuoteID,
		ObservationID: "f7204e49-73df-4657-8d92-fa315ec4729f", Outcome: "filled",
		ObservedAt: order.CreatedAt, ReceivedAt: order.CreatedAt,
		QuantityRaw: "1", GrossUSD: order.GrossUSD, FeeUSD: "0",
	}
	raw, err := json.MarshalIndent(observed, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	if _, _, err := client.ReconcileDocument(ctx, append(bytes.Clone(raw), '\n'), digest); !errors.Is(err, accounts.ErrInvalid) {
		t.Fatalf("owner accepted a mismatched observation digest: %v", err)
	}
	if settled, recorded, err := client.ReconcileDocument(ctx, raw, digest); err != nil || !recorded || settled.Status != "filled" {
		t.Fatalf("reconcile: status=%s new=%t err=%v", settled.Status, recorded, err)
	}
	if _, recorded, err := client.ReconcileDocument(ctx, raw, digest); err != nil || recorded {
		t.Fatalf("reconciliation repeat: new=%t err=%v", recorded, err)
	}
}

func accountHTTPDatabase(t *testing.T) (context.Context, *accounts.Store) {
	t.Helper()
	dsn := os.Getenv("TRADINGBOT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TRADINGBOT_TEST_DATABASE_URL is required for real PostgreSQL tests")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	pg, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("connect to the test PostgreSQL database")
	}
	schema := "account_http_" + strings.ToLower(rand.Text())
	identifier := pgx.Identifier{schema}.Sanitize()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		defer pg.Close(ctx)
		if _, err := pg.Exec(ctx, "DROP SCHEMA IF EXISTS "+identifier+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	if _, err := pg.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Scheme == "" {
		t.Fatal("test PostgreSQL DSN must be a URI")
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	store, err := accounts.Open(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	return ctx, store
}
