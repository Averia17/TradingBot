package jupiter_rehearsal

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"tradingbot/venue-quotes/internal/jupiter"
	"tradingbot/venue-quotes/internal/jupiter_execution"
	"tradingbot/venue-quotes/internal/jupiter_http"
	"tradingbot/venue-quotes/internal/quotes"
	"tradingbot/venue-quotes/internal/solana_balance"
)

const executorToken = "jupiter-rehearsal-only-executor-token"
const buyIntent = "7c4d4963-94e7-498b-8869-4e46feacbdd0"
const sellIntent = "0bff9408-627d-4e44-8a9d-2051724ac624"

func TestJupiterPaperRehearsal(t *testing.T) {
	paper := startPaper(t)
	signer := newSigner(t)
	venue := newVenue(signer.owner())
	adapter := startAdapter(t, paper.dsn, signer, venue)

	initial := paper.create()
	if initial.CashUSD != "1000" || initial.ReservedCashUSD != "0" {
		t.Fatalf("unexpected initial paper state: %+v", initial)
	}
	price := adapterCall[jupiter.Price](t, adapter, http.MethodGet, "/v1/price?mint="+fixtureAssetMint, nil, false, http.StatusOK)
	if price.USDPrice != "2.1" || price.Decimals != 0 {
		t.Fatalf("unexpected fixture price: %+v", price)
	}
	before := adapterBalances(t, adapter)
	verifyWallet(t, before, 1_000_000_000, 0, 20_000_000)
	reads := venue.balanceReads()
	if cached := adapterBalances(t, adapter); !reflect.DeepEqual(cached, before) || venue.balanceReads() != reads {
		t.Fatal("wallet snapshot was not served from cache")
	}

	buy := jupiter.SwapRequest{Side: "BUY", AssetMint: fixtureAssetMint, InAmount: "200000000", MinOutAmount: "100", SlippageBPS: 1_000}
	buyPrepared := prepare(t, adapter, buy, fixtureUSDC, fixtureAssetMint, "110")
	buyOrder := paper.reserve(buy.Side, buy.MinOutAmount, "200")
	if reserved := paper.account(); reserved.ReservedCashUSD != "200" || reserved.CashUSD != "1000" {
		t.Fatalf("BUY did not reserve exact USDC equivalent: %+v", reserved)
	}
	buyApproval := approve(t, signer, buyPrepared, buy, buyIntent, buyOrder, "200")
	adapterCall[struct{}](t, adapter, http.MethodPost, "/v1/swaps/execute", buyApproval, false, http.StatusUnauthorized)
	if venue.executions() != 0 {
		t.Fatal("uncredentialed call reached the provider")
	}
	buyReceipt := dispatch(t, adapter, buyApproval, buyOrder, buyIntent, "unknown")
	buyOrder = paper.markUnknown(buyOrder)
	if buyOrder.Status != "unknown" {
		t.Fatalf("paper order lost the ambiguous outcome: %+v", buyOrder)
	}
	if reserved := paper.account(); reserved.ReservedCashUSD != "200" || reserved.CashUSD != "1000" {
		t.Fatalf("timeout changed paper cash or released its reservation: %+v", reserved)
	}
	requirePending(t, adapter, buyIntent, buyReceipt.Signature)

	adapter.close()
	adapter = startAdapter(t, paper.dsn, signer, venue)
	replay := dispatch(t, adapter, buyApproval, buyOrder, buyIntent, "unknown")
	if replay.Signature != buyReceipt.Signature || !replay.ClaimedAt.Equal(buyReceipt.ClaimedAt) || venue.executions() != 1 {
		t.Fatalf("restart failed to recover the original attempt: %+v", replay)
	}
	requirePending(t, adapter, buyIntent, buyReceipt.Signature)
	pendingBuy := adapterBalances(t, adapter)
	verifyWallet(t, pendingBuy, 1_000_000_000, 0, 20_000_000)
	buyStatus, afterBuy := finalize(t, venue, adapter, buyIntent, buyReceipt.Signature)
	verifyWallet(t, afterBuy, 800_000_000, 100, 19_995_000)
	buyQuantity, buyGross := observedFill(t, buy, pendingBuy, afterBuy, buyStatus)
	if buyQuantity != "100" || buyGross != "200" || buyPrepared.OutAmount == buyQuantity {
		t.Fatalf("BUY did not use post-finality amounts: qty=%s gross=%s", buyQuantity, buyGross)
	}
	if settled := paper.settle(buyOrder, buyQuantity, buyGross, true); settled.Status != "filled" {
		t.Fatalf("BUY failed paper reconciliation: %+v", settled)
	}
	bought := paper.account()
	if bought.CashUSD != "800" || bought.ReservedCashUSD != "0" || len(bought.Positions) != 1 || bought.Positions[0].QuantityRaw != "100" {
		t.Fatalf("BUY settlement disagrees with the finalized wallet: %+v", bought)
	}
	t.Log("BUY: timeout retained the 200 USDC reserve, restart recovered the attempt, and finality booked 100 fixture units once")

	sell := jupiter.SwapRequest{Side: "SELL", AssetMint: fixtureAssetMint, InAmount: "100", MinOutAmount: "210000000", SlippageBPS: 1_000}
	sellPrepared := prepare(t, adapter, sell, fixtureAssetMint, fixtureUSDC, "220000000")
	sellOrder := paper.reserve(sell.Side, sell.InAmount, "210")
	reservedSell := paper.account()
	if len(reservedSell.Positions) != 1 || reservedSell.Positions[0].ReservedQuantityRaw != "100" || reservedSell.CashUSD != "800" {
		t.Fatalf("SELL did not hold exact token units: %+v", reservedSell)
	}
	sellApproval := approve(t, signer, sellPrepared, sell, sellIntent, sellOrder, "210")
	sellReceipt := dispatch(t, adapter, sellApproval, sellOrder, sellIntent, "accepted")
	requirePending(t, adapter, sellIntent, sellReceipt.Signature)
	if pending := paper.account(); pending.CashUSD != "800" || len(pending.Positions) != 1 || pending.Positions[0].ReservedQuantityRaw != "100" {
		t.Fatalf("provisional Jupiter acknowledgement booked or released a SELL: %+v", pending)
	}
	pendingSell := adapterBalances(t, adapter)
	verifyWallet(t, pendingSell, 800_000_000, 100, 19_995_000)
	sellStatus, afterSell := finalize(t, venue, adapter, sellIntent, sellReceipt.Signature)
	verifyWallet(t, afterSell, 1_015_000_000, 0, 19_990_000)
	sellQuantity, sellGross := observedFill(t, sell, pendingSell, afterSell, sellStatus)
	if sellQuantity != "100" || sellGross != "215" || sellPrepared.OutAmount == "215000000" {
		t.Fatalf("SELL did not use post-finality amounts: qty=%s gross=%s", sellQuantity, sellGross)
	}
	if settled := paper.settle(sellOrder, sellQuantity, sellGross, false); settled.Status != "filled" {
		t.Fatalf("SELL failed paper settlement: %+v", settled)
	}
	if settled := paper.account(); settled.CashUSD != "1015" || settled.ReservedCashUSD != "0" || len(settled.Positions) != 0 {
		t.Fatalf("round-trip paper settlement disagrees with finalized wallet: %+v", settled)
	}
	if venue.executions() != 2 || venue.unexpectedRequests() != 0 {
		t.Fatalf("unexpected provider calls: execute=%d unknown=%d", venue.executions(), venue.unexpectedRequests())
	}
	t.Log("SELL: the token reserve survived a provisional response; final settlement credited 215 USDC, ended at 1015 USDC and charged 10000 fixture lamports total")
}

type rehearsalAdapter struct {
	handler http.Handler
	close   func()
}

func startAdapter(t *testing.T, dsn string, signer *fixtureSigner, venue *fixtureVenue) rehearsalAdapter {
	t.Helper()
	market := jupiter.NewWithTransport(fixtureAPIKey, venue)
	chain, err := solana_balance.NewWithTransport(fixtureRPCURL, signer.owner(), 2*time.Minute, venue)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := jupiter_execution.OpenPostgres(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	closeAdapter := func() {
		once.Do(func() {
			journal.Close()
			chain.CloseIdleConnections()
			market.CloseIdleConnections()
		})
	}
	t.Cleanup(closeAdapter)
	execution, err := jupiter_execution.New(journal, market, chain, signer.owner(), signer.policyHash, signer.approvalKey)
	if err != nil {
		t.Fatal(err)
	}
	handler := jupiter_http.Handler(market, chain, signer.owner())
	jupiter_http.RegisterTransactions(handler, chain, execution, executorToken)
	return rehearsalAdapter{handler: handler, close: closeAdapter}
}

func adapterCall[T any](t *testing.T, adapter rehearsalAdapter, method, path string, body any, authorized bool, status int) T {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if authorized {
		req.Header.Set("Authorization", "Bearer "+executorToken)
	}
	response := httptest.NewRecorder()
	adapter.handler.ServeHTTP(response, req)
	var output T
	if response.Code != status || json.Unmarshal(response.Body.Bytes(), &output) != nil {
		t.Fatalf("%s %s returned %d (wanted %d): %s", method, path, response.Code, status, response.Body.String())
	}
	return output
}

func prepare(t *testing.T, adapter rehearsalAdapter, swap jupiter.SwapRequest, inputMint, outputMint, expectedOut string) jupiter.PreparedSwap {
	t.Helper()
	quote := adapterCall[quotes.Observation](t, adapter, http.MethodPost, "/v1/quotes",
		quotes.Request{Network: quotes.SolanaMainnet, InputMint: inputMint, OutputMint: outputMint, InAmount: swap.InAmount}, false, http.StatusOK)
	if quote.Firmness != "indicative" || quote.OutAmount != expectedOut || quote.MinOutAmount != swap.MinOutAmount {
		t.Fatalf("unexpected sizing quote: %+v", quote)
	}
	prepared := adapterCall[jupiter.PreparedSwap](t, adapter, http.MethodPost, "/v1/swaps/prepare", swap, false, http.StatusOK)
	if prepared.InputMint != inputMint || prepared.OutputMint != outputMint || prepared.OutAmount != quote.OutAmount ||
		prepared.MinOutAmount != quote.MinOutAmount || prepared.State != "prepared" {
		t.Fatalf("prepared swap differs from approved quote terms: %+v", prepared)
	}
	return prepared
}

func approve(t *testing.T, signer *fixtureSigner, prepared jupiter.PreparedSwap, swap jupiter.SwapRequest, intent string, order paperOrder, grossLimit string) jupiter_execution.Request {
	t.Helper()
	quantity := swap.InAmount
	if swap.Side == "BUY" {
		quantity = swap.MinOutAmount
	}
	if order.Status != "reserved" || order.Side != swap.Side || order.QuantityRaw != quantity || order.GrossUSD != grossLimit {
		t.Fatalf("paper reservation does not cover the swap: %+v", order)
	}
	request, err := signer.approve(prepared, swap, intent, order.OrderID, order.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func dispatch(t *testing.T, adapter rehearsalAdapter, approval jupiter_execution.Request, order paperOrder, intent, providerStatus string) jupiter_execution.Receipt {
	t.Helper()
	receipt := adapterCall[jupiter_execution.Receipt](t, adapter, http.MethodPost, "/v1/swaps/execute", approval, true, http.StatusAccepted)
	if receipt.ProviderStatus != providerStatus || receipt.ChainStatus != nil || receipt.IntentID != intent ||
		receipt.ReservationID != order.OrderID || receipt.Signature == "" || receipt.ClaimedAt.IsZero() {
		t.Fatalf("unexpected attempt receipt: %+v", receipt)
	}
	return receipt
}

func requirePending(t *testing.T, adapter rehearsalAdapter, intent, signature string) {
	t.Helper()
	receipt := adapterCall[jupiter_execution.Receipt](t, adapter, http.MethodGet, "/v1/swaps/attempts/"+intent, nil, true, http.StatusOK)
	if receipt.Signature != signature || receipt.ChainStatus == nil || receipt.ChainStatus.State != "unknown" {
		t.Fatalf("attempt unexpectedly settled: %+v", receipt)
	}
}

func finalize(t *testing.T, venue *fixtureVenue, adapter rehearsalAdapter, intent, signature string) (solana_balance.TransactionStatus, solana_balance.Snapshot) {
	t.Helper()
	if err := venue.finalize(signature); err != nil {
		t.Fatal(err)
	}
	status := adapterCall[solana_balance.TransactionStatus](t, adapter, http.MethodGet, "/v1/swaps/status?signature="+signature, nil, false, http.StatusOK)
	receipt := adapterCall[jupiter_execution.Receipt](t, adapter, http.MethodGet, "/v1/swaps/attempts/"+intent, nil, true, http.StatusOK)
	if status.State != "finalized_success" || status.Signature != signature || status.FeeLamports == nil || *status.FeeLamports != "5000" ||
		status.TransactionSlot == nil || receipt.ChainStatus == nil || receipt.ChainStatus.State != status.State ||
		receipt.ChainStatus.Signature != signature || receipt.ChainStatus.TransactionSlot == nil ||
		*receipt.ChainStatus.TransactionSlot != *status.TransactionSlot || receipt.ChainStatus.FeeLamports == nil ||
		*receipt.ChainStatus.FeeLamports != *status.FeeLamports {
		t.Fatalf("status and durable attempt disagree: status=%+v receipt=%+v", status, receipt)
	}
	reads := venue.balanceReads()
	wallet := adapterBalances(t, adapter)
	if venue.balanceReads() != reads+1 || wallet.Native.Slot < *status.TransactionSlot {
		t.Fatalf("finality did not refresh the wallet: %+v", wallet)
	}
	for _, program := range wallet.TokenPrograms {
		if program.Slot < *status.TransactionSlot {
			t.Fatalf("wallet token program predates finality: %+v", program)
		}
	}
	return status, wallet
}

func adapterBalances(t *testing.T, adapter rehearsalAdapter) solana_balance.Snapshot {
	t.Helper()
	return adapterCall[solana_balance.Snapshot](t, adapter, http.MethodGet, "/v1/balances", nil, false, http.StatusOK)
}

type walletUnits struct {
	usdc, asset, sol uint64
}

func verifyWallet(t *testing.T, snapshot solana_balance.Snapshot, usdc, asset, sol uint64) {
	t.Helper()
	if got, want := units(t, snapshot), (walletUnits{usdc: usdc, asset: asset, sol: sol}); got != want {
		t.Fatalf("unexpected synthetic wallet: got=%+v want=%+v", got, want)
	}
}

func units(t *testing.T, wallet solana_balance.Snapshot) walletUnits {
	t.Helper()
	values := walletUnits{sol: parseUnits(t, wallet.Native.AmountLamports)}
	var foundUSDC, foundAsset bool
	for _, program := range wallet.TokenPrograms {
		for _, account := range program.Accounts {
			switch {
			case account.Mint == fixtureUSDC && account.Decimals == 6 && !foundUSDC:
				values.usdc, foundUSDC = parseUnits(t, account.AmountRaw), true
			case account.Mint == fixtureAssetMint && account.Decimals == 0 && !foundAsset:
				values.asset, foundAsset = parseUnits(t, account.AmountRaw), true
			default:
				t.Fatal("unexpected or duplicate token account in fixture wallet")
			}
		}
	}
	if !foundUSDC {
		t.Fatal("fixture wallet omitted USDC")
	}
	return values
}

func observedFill(t *testing.T, swap jupiter.SwapRequest, before, after solana_balance.Snapshot, status solana_balance.TransactionStatus) (quantity, gross string) {
	t.Helper()
	from, to := units(t, before), units(t, after)
	if from.sol <= to.sol || from.sol-to.sol != parseUnits(t, *status.FeeLamports) {
		t.Fatal("fee did not match the synthetic native balance change")
	}
	var asset, usdc uint64
	switch swap.Side {
	case "BUY":
		if from.usdc <= to.usdc || from.asset >= to.asset {
			t.Fatal("wrong BUY asset flow")
		}
		asset, usdc = to.asset-from.asset, from.usdc-to.usdc
		if usdc != parseUnits(t, swap.InAmount) || asset < parseUnits(t, swap.MinOutAmount) {
			t.Fatal("BUY settlement outside authorized bounds")
		}
	case "SELL":
		if from.asset <= to.asset || from.usdc >= to.usdc {
			t.Fatal("wrong SELL asset flow")
		}
		asset, usdc = from.asset-to.asset, to.usdc-from.usdc
		if asset != parseUnits(t, swap.InAmount) || usdc < parseUnits(t, swap.MinOutAmount) {
			t.Fatal("SELL settlement outside authorized bounds")
		}
	default:
		t.Fatal("unknown fixture side")
	}
	// This fixture models USDC at synthetic USD par. Native SOL fees are
	// observed independently and never deducted from the USDC balance.
	if usdc%1_000_000 != 0 {
		t.Fatal("fixture expected whole USDC")
	}
	return strconv.FormatUint(asset, 10), strconv.FormatUint(usdc/1_000_000, 10)
}

func parseUnits(t *testing.T, raw string) uint64 {
	t.Helper()
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || strconv.FormatUint(value, 10) != raw {
		t.Fatalf("invalid fixture amount %q", raw)
	}
	return value
}
