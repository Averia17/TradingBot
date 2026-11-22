package jupiter_rehearsal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"tradingbot/venue-quotes/internal/jupiter"
	"tradingbot/venue-quotes/internal/jupiter_execution"
	"tradingbot/venue-quotes/internal/jupiter_http"
	"tradingbot/venue-quotes/internal/quotes"
	"tradingbot/venue-quotes/internal/solana_balance"
)

var scheduledSchema = regexp.MustCompile(`^jupiter_scheduled_[0-9a-z]{1,40}$`)

type scheduledOrder struct {
	SchemaVersion string `json:"schema_version"`
	SourceMode    string `json:"source_mode"`
	AccountID     string `json:"account_id"`
	JobID         string `json:"job_id"`
	OrderID       string `json:"order_id"`
	QuoteID       string `json:"quote_id"`
	Instrument    struct {
		ID              string `json:"instrument_id"`
		RegistryVersion string `json:"registry_version"`
		Decimals        *uint8 `json:"decimals"`
	} `json:"instrument"`
	Side          string    `json:"side"`
	QuantityRaw   string    `json:"quantity_raw"`
	GrossLimitUSD string    `json:"gross_limit_usd"`
	GrossUSD      string    `json:"gross_usd"`
	FeeUSD        string    `json:"fee_usd"`
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     time.Time `json:"expires_at"`
}

type scheduledSettlement struct {
	SchemaVersion string     `json:"schema_version"`
	SourceMode    string     `json:"source_mode"`
	RequestSHA256 string     `json:"request_sha256"`
	Status        string     `json:"status"`
	QuantityRaw   string     `json:"quantity_raw,omitempty"`
	GrossUSD      string     `json:"gross_usd,omitempty"`
	FeeUSD        string     `json:"fee_usd,omitempty"`
	ObservedAt    *time.Time `json:"observed_at,omitempty"`
}

type scheduledAttempt struct {
	order       scheduledOrder
	receipt     jupiter_execution.Receipt
	settlement  scheduledSettlement
	before      walletUnits
	after       walletUnits
	observedAt  *time.Time
	pendingSent bool
}

type scheduledVenue struct {
	mu       sync.Mutex
	dsn      string
	signer   *fixtureSigner
	venue    *fixtureVenue
	adapter  rehearsalAdapter
	attempts map[string]*scheduledAttempt

	accountID string
	buy       *scheduledAttempt
	sell      *scheduledAttempt
	restarts  int
}

// TestScheduledJupiterFixtureServer runs only as a child of the scheduled
// rehearsal. The provider and RPC are in-process transports that never dial.
func TestScheduledJupiterFixtureServer(t *testing.T) {
	if os.Getenv("TRADINGBOT_JUPITER_FIXTURE_SERVER") != "1" {
		t.Skip("scheduled fixture server only runs as an explicit test child")
	}
	for _, name := range []string{"PGHOST", "PGHOSTADDR", "PGPORT", "PGDATABASE", "PGSERVICE", "PGSERVICEFILE", "PGOPTIONS", "PGTARGETSESSIONATTRS"} {
		if os.Getenv(name) != "" {
			t.Fatalf("clear %s when starting the scheduled fixture server", name)
		}
	}
	dsn, err := scheduledDatabaseURL(os.Getenv("TRADINGBOT_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	signer := newSigner(t)
	fixture := &scheduledVenue{dsn: dsn, signer: signer, venue: newVenue(signer.owner()), attempts: make(map[string]*scheduledAttempt, 2)}
	if err := fixture.connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() { fixture.adapter.close() }()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal("bind scheduled fixture server to loopback")
	}
	defer listener.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeScheduledJSON(w, http.StatusOK, map[string]string{"status": "ok", "source_mode": "synthetic_fixture"})
	})
	mux.HandleFunc("GET /v1/paper-fixture/evidence", fixture.evidence)
	mux.HandleFunc("POST /v1/paper-fixture/settle", fixture.settle)
	ctx, stop := signal.NotifyContext(t.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := &http.Server{
		Handler: mux, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 15 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	fmt.Printf("JUPITER_FIXTURE_PORT=%d\n", listener.Addr().(*net.TCPAddr).Port)
	err = server.Serve(listener)
	stop()
	<-stopped
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatal("scheduled fixture server stopped unexpectedly")
	}
}

func TestScheduledFixtureRequiresAnIsolatedLocalSchema(t *testing.T) {
	const allowed = "postgresql://fixture@127.0.0.1:5432/test?search_path=jupiter_scheduled_b4"
	if _, err := scheduledDatabaseURL(allowed); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		"postgresql://fixture@127.0.0.1:5432/test",
		"postgresql://fixture@127.0.0.1:5432/test?search_path=public",
		"postgresql://fixture@127.0.0.1:5432/test?search_path=jupiter_scheduled_b4,public",
		"postgresql://fixture@localhost:5432/test?search_path=jupiter_scheduled_b4",
		allowed + "&search_path=jupiter_scheduled_b4",
		allowed + "&host=database.example",
	} {
		if _, err := scheduledDatabaseURL(raw); err == nil {
			t.Fatal("scheduled fixture accepted a non-isolated connection")
		}
	}
}

func scheduledDatabaseURL(raw string) (string, error) {
	invalid := errors.New("scheduled fixture requires explicit numeric loopback PostgreSQL and an isolated jupiter_scheduled schema")
	uri, err := url.Parse(raw)
	if err != nil {
		return "", invalid
	}
	query, err := url.ParseQuery(uri.RawQuery)
	if err != nil || len(query["search_path"]) != 1 || !scheduledSchema.MatchString(query.Get("search_path")) {
		return "", invalid
	}
	schema := query.Get("search_path")
	query.Del("search_path")
	uri.RawQuery = query.Encode()
	checked, err := localPaperDatabaseURL(uri.String())
	if err != nil {
		return "", invalid
	}
	query = checked.Query()
	query.Set("search_path", schema)
	checked.RawQuery = query.Encode()
	return checked.String(), nil
}

func (f *scheduledVenue) connect(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	market := jupiter.NewWithTransport(fixtureAPIKey, f.venue)
	chain, err := solana_balance.NewWithTransport(fixtureRPCURL, f.signer.owner(), 2*time.Minute, f.venue)
	if err != nil {
		return errors.New("construct fixture Solana reader")
	}
	journal, err := jupiter_execution.OpenPostgres(ctx, f.dsn)
	if err != nil {
		return errors.New("open scheduled fixture PostgreSQL attempt journal")
	}
	executor, err := jupiter_execution.New(journal, market, chain, f.signer.owner(), f.signer.policyHash, f.signer.approvalKey)
	if err != nil {
		journal.Close()
		return errors.New("construct scheduled fixture executor")
	}
	handler := jupiter_http.Handler(market, chain, f.signer.owner())
	jupiter_http.RegisterTransactions(handler, chain, executor, executorToken)
	if f.adapter.close != nil {
		f.adapter.close()
		f.restarts++
	}
	f.adapter = rehearsalAdapter{handler: handler, close: func() {
		journal.Close()
		chain.CloseIdleConnections()
		market.CloseIdleConnections()
	}}
	return nil
}

func (f *scheduledVenue) settle(w http.ResponseWriter, r *http.Request) {
	media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if media != "application/json" || r.URL.RawQuery != "" {
		writeScheduledJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_fixture_request"})
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	var order scheduledOrder
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err != nil || !json.Valid(raw) || decoder.Decode(&order) != nil || !order.valid() {
		writeScheduledJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_fixture_request"})
		return
	}
	digest := sha256.Sum256(raw)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	f.mu.Lock()
	defer f.mu.Unlock()
	result, status := f.apply(ctx, order, hex.EncodeToString(digest[:]))
	if status != http.StatusAccepted && status != http.StatusOK {
		writeScheduledJSON(w, status, map[string]string{"error": "scheduled_fixture_rejected"})
		return
	}
	writeScheduledJSON(w, status, result)
}

func (o scheduledOrder) valid() bool {
	if o.SchemaVersion != "0.1" || o.SourceMode != "synthetic_fixture" || !validFixtureID(o.AccountID) ||
		!validFixtureID(o.JobID) || !validFixtureID(o.OrderID) || !validFixtureID(o.QuoteID) ||
		o.Instrument.ID != "fixture:Jupiter" || o.Instrument.RegistryVersion != "fixture-v1" ||
		o.Instrument.Decimals == nil || *o.Instrument.Decimals != 0 || o.QuantityRaw != "100" || o.FeeUSD != "0" ||
		o.CreatedAt.IsZero() || o.CreatedAt.After(time.Now()) || !o.ExpiresAt.After(o.CreatedAt) {
		return false
	}
	return (o.Side == "BUY" && o.GrossLimitUSD == "200" && o.GrossUSD == "200") ||
		(o.Side == "SELL" && o.GrossLimitUSD == "210" && o.GrossUSD == "210")
}

func (f *scheduledVenue) apply(ctx context.Context, order scheduledOrder, digest string) (scheduledSettlement, int) {
	attempt := f.attempts[order.OrderID]
	if attempt == nil {
		if !order.ExpiresAt.After(time.Now()) {
			return scheduledSettlement{}, http.StatusUnprocessableEntity
		}
		if (f.accountID != "" && order.AccountID != f.accountID) ||
			(order.Side == "BUY" && f.buy != nil) ||
			(order.Side == "SELL" && (f.sell != nil || f.buy == nil || f.buy.settlement.Status != "finalized" ||
				f.buy.order.JobID == order.JobID || f.buy.order.QuoteID == order.QuoteID)) {
			return scheduledSettlement{}, http.StatusConflict
		}
		var err error
		attempt, err = f.submit(ctx, order, digest)
		if err != nil {
			return scheduledSettlement{}, http.StatusServiceUnavailable
		}
		f.attempts[order.OrderID], f.accountID = attempt, order.AccountID
		if order.Side == "BUY" {
			f.buy = attempt
		} else {
			f.sell = attempt
		}
	}
	if attempt.settlement.RequestSHA256 != digest {
		return scheduledSettlement{}, http.StatusConflict
	}
	if attempt.settlement.Status == "finalized" {
		return attempt.settlement, http.StatusOK
	}
	if !attempt.pendingSent {
		if order.Side == "BUY" && f.restarts == 0 && f.connect(ctx) != nil {
			return scheduledSettlement{}, http.StatusServiceUnavailable
		}
		attempt.pendingSent = true
		return attempt.settlement, http.StatusAccepted
	}
	if err := f.finalize(ctx, attempt); err != nil {
		return scheduledSettlement{}, http.StatusServiceUnavailable
	}
	return attempt.settlement, http.StatusOK
}

func (f *scheduledVenue) submit(ctx context.Context, order scheduledOrder, digest string) (*scheduledAttempt, error) {
	fixture := venueOrders[0]
	providerStatus := "unknown"
	wantBefore := walletUnits{usdc: 1_000_000_000, sol: 20_000_000}
	if order.Side == "SELL" {
		fixture = venueOrders[1]
		providerStatus = "accepted"
		wantBefore = walletUnits{usdc: 800_000_000, asset: 100, sol: 19_995_000}
	}
	before, err := scheduledBalances(ctx, f.adapter)
	if err != nil || before != wantBefore {
		return nil, errors.New("unexpected fixture opening balance")
	}
	quote, err := scheduledAdapterCall[quotes.Observation](ctx, f.adapter, http.MethodPost, "/v1/quotes", quotes.Request{
		Network: quotes.SolanaMainnet, InputMint: fixture.inputMint, OutputMint: fixture.outputMint, InAmount: rawAmount(fixture.input),
	}, false, http.StatusOK)
	if err != nil || quote.Firmness != "indicative" || quote.ProviderRequestID != fixture.id ||
		quote.OutAmount != rawAmount(fixture.quoted) || quote.MinOutAmount != rawAmount(fixture.floor) {
		return nil, errors.New("unexpected fixture quote")
	}
	swap := jupiter.SwapRequest{Side: order.Side, AssetMint: fixtureAssetMint, InAmount: quote.InAmount, MinOutAmount: quote.MinOutAmount, SlippageBPS: 1000}
	prepared, err := scheduledAdapterCall[jupiter.PreparedSwap](ctx, f.adapter, http.MethodPost, "/v1/swaps/prepare", swap, false, http.StatusOK)
	if err != nil {
		return nil, err
	}
	request, err := f.signer.approve(prepared, swap, order.JobID, order.OrderID, order.ExpiresAt)
	if err != nil {
		return nil, err
	}
	receipt, err := scheduledAdapterCall[jupiter_execution.Receipt](ctx, f.adapter, http.MethodPost, "/v1/swaps/execute", request, true, http.StatusAccepted)
	if err != nil || receipt.ProviderStatus != providerStatus || receipt.IntentID != order.JobID ||
		receipt.ChainStatus != nil || receipt.ReservationID != order.OrderID || receipt.Signature == "" || receipt.ClaimedAt.IsZero() {
		return nil, errors.New("fixture execution was not recorded")
	}
	return &scheduledAttempt{order: order, receipt: receipt, before: before, settlement: scheduledSettlement{
		SchemaVersion: "0.1", SourceMode: "synthetic_fixture", RequestSHA256: digest, Status: "pending",
	}}, nil
}

func (f *scheduledVenue) finalize(ctx context.Context, attempt *scheduledAttempt) error {
	path := "/v1/swaps/attempts/" + attempt.order.JobID
	receipt, err := scheduledAdapterCall[jupiter_execution.Receipt](ctx, f.adapter, http.MethodGet, path, nil, true, http.StatusOK)
	if err != nil || receipt.Signature != attempt.receipt.Signature || !receipt.ClaimedAt.Equal(attempt.receipt.ClaimedAt) ||
		receipt.ProviderStatus != attempt.receipt.ProviderStatus || receipt.ReservationID != attempt.order.OrderID || receipt.ChainStatus == nil {
		return errors.New("fixture could not recover the durable attempt")
	}
	if attempt.observedAt == nil {
		if receipt.ChainStatus.State != "unknown" || f.venue.finalize(receipt.Signature) != nil {
			return errors.New("fixture could not finalize a pending attempt")
		}
		attempt.observedAt = new(time.Now().UTC().Truncate(time.Microsecond))
	}
	reading, err := scheduledAdapterCall[jupiter_execution.Receipt](ctx, f.adapter, http.MethodGet, path, nil, true, http.StatusOK)
	if err != nil || reading.Signature != receipt.Signature || reading.ChainStatus == nil ||
		reading.ChainStatus.State != "finalized_success" || reading.ChainStatus.Signature != reading.Signature ||
		reading.ChainStatus.TransactionSlot == nil || reading.ChainStatus.FeeLamports == nil || *reading.ChainStatus.FeeLamports != "5000" {
		return errors.New("fixture did not observe expected finality")
	}
	reads := f.venue.balanceReads()
	before, after := attempt.before, walletUnits{}
	snapshot, err := scheduledAdapterCall[solana_balance.Snapshot](ctx, f.adapter, http.MethodGet, "/v1/balances", nil, false, http.StatusOK)
	if err != nil || f.venue.balanceReads() != reads+1 || snapshot.Native.Slot < *reading.ChainStatus.TransactionSlot {
		return errors.New("fixture did not refresh its finalized balance")
	}
	after, err = scheduledUnits(snapshot, *reading.ChainStatus.TransactionSlot)
	if err != nil || before.sol < after.sol || before.sol-after.sol != fixtureFee {
		return errors.New("unexpected fixture fee and balances")
	}
	quantity, gross := uint64(0), uint64(0)
	switch {
	case attempt.order.Side == "BUY" && before.usdc > after.usdc && before.asset < after.asset:
		quantity, gross = after.asset-before.asset, before.usdc-after.usdc
		if quantity != 100 || gross != 200_000_000 {
			return errors.New("unexpected fixture BUY fill")
		}
	case attempt.order.Side == "SELL" && before.usdc < after.usdc && before.asset > after.asset:
		quantity, gross = before.asset-after.asset, after.usdc-before.usdc
		if quantity != 100 || gross != 215_000_000 {
			return errors.New("unexpected fixture SELL fill")
		}
	default:
		return errors.New("unexpected fixture settlement direction")
	}
	attempt.settlement.Status = "finalized"
	attempt.settlement.QuantityRaw = rawAmount(quantity)
	attempt.settlement.GrossUSD = rawAmount(gross / 1_000_000)
	attempt.settlement.FeeUSD = "0"
	attempt.settlement.ObservedAt = attempt.observedAt
	attempt.after = after
	return nil
}

func scheduledBalances(ctx context.Context, adapter rehearsalAdapter) (walletUnits, error) {
	snapshot, err := scheduledAdapterCall[solana_balance.Snapshot](ctx, adapter, http.MethodGet, "/v1/balances", nil, false, http.StatusOK)
	if err != nil {
		return walletUnits{}, err
	}
	return scheduledUnits(snapshot, 0)
}

func scheduledUnits(snapshot solana_balance.Snapshot, floor uint64) (walletUnits, error) {
	invalid := errors.New("invalid scheduled fixture wallet balances")
	lamports, err := strconv.ParseUint(snapshot.Native.AmountLamports, 10, 64)
	if err != nil || snapshot.Native.Slot < floor {
		return walletUnits{}, invalid
	}
	value := walletUnits{sol: lamports}
	seenUSDC, seenAsset := false, false
	for _, program := range snapshot.TokenPrograms {
		if program.Slot < floor {
			return walletUnits{}, invalid
		}
		for _, account := range program.Accounts {
			amount, err := strconv.ParseUint(account.AmountRaw, 10, 64)
			if err != nil {
				return walletUnits{}, invalid
			}
			switch {
			case account.Mint == fixtureUSDC && account.Decimals == 6 && !seenUSDC:
				value.usdc, seenUSDC = amount, true
			case account.Mint == fixtureAssetMint && account.Decimals == 0 && !seenAsset:
				value.asset, seenAsset = amount, true
			default:
				return walletUnits{}, invalid
			}
		}
	}
	if !seenUSDC || !seenAsset {
		return walletUnits{}, invalid
	}
	return value, nil
}

func (f *scheduledVenue) evidence(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.venue.mu.Lock()
	defer f.venue.mu.Unlock()
	var postBuyUSDC string
	if f.buy != nil && f.buy.settlement.Status == "finalized" {
		postBuyUSDC = rawAmount(f.buy.after.usdc)
	}
	writeScheduledJSON(w, http.StatusOK, map[string]any{
		"schema_version": "0.1", "source_mode": "synthetic_fixture", "send_count": f.venue.executeCalls,
		"unexpected_requests": f.venue.rejected, "restart_count": f.restarts, "initial_usdc_raw": "1000000000",
		"post_buy_usdc_raw": postBuyUSDC, "final_usdc_raw": rawAmount(f.venue.usdc), "final_asset_raw": rawAmount(f.venue.asset),
		"initial_lamports": "20000000", "final_lamports": rawAmount(f.venue.lamports),
	})
}

func scheduledAdapterCall[T any](ctx context.Context, adapter rehearsalAdapter, method, path string, payload any, private bool, status int) (T, error) {
	var zero T
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return zero, err
		}
	}
	request := httptest.NewRequestWithContext(ctx, method, path, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if private {
		request.Header.Set("Authorization", "Bearer "+executorToken)
	}
	response := httptest.NewRecorder()
	adapter.handler.ServeHTTP(response, request)
	var output T
	if response.Code != status || json.Unmarshal(response.Body.Bytes(), &output) != nil {
		return zero, errors.New("unexpected local Jupiter Adapter response")
	}
	return output, nil
}

func writeScheduledJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}
