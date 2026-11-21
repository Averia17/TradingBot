package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/control"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paper_memory"
	"tradingbot/control/internal/paper_risk"
	"tradingbot/control/internal/paper_sim"
	"tradingbot/control/internal/paper_thesis"
	"tradingbot/control/internal/paper_workflow"
	"tradingbot/control/internal/research_scheduler"
)

func TestCollectedFixtureQuoteFlowsThroughStablePaperDispatch(t *testing.T) {
	if os.Getenv("TRADINGBOT_TEST_DATABASE_URL") == "" {
		t.Skip("TRADINGBOT_TEST_DATABASE_URL is required for real PostgreSQL tests")
	}
	bin := t.TempDir()
	extension := ""
	if runtime.GOOS == "windows" {
		extension = ".exe"
	}
	build := exec.CommandContext(t.Context(), "go", "build", "-mod=readonly", "-o", bin,
		"../paper-quote-collect", "../paper-quote-pin")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the collection and quote pin commands: %v\n%s", err, output)
	}
	f := newStableEntryFixture(t, nil)
	registry := f.config.Identity.Registry
	// This is an explicitly dated synthetic replay. Its observations become
	// usable only after actual research completion and the PostgreSQL clock.
	buyAt := f.config.At.Add(10 * time.Second)
	sellAt := f.config.At.Add(22 * time.Second)
	cutoff := sellAt.Add(time.Second)
	var mismatch atomic.Bool
	var calls atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		defer r.Body.Close()
		var input map[string]string
		if r.Method != http.MethodPost || json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&input) != nil {
			t.Errorf("collector did not POST a fixture request: %s %+v", r.Method, input)
			http.Error(w, "unexpected fixture request", http.StatusBadRequest)
			return
		}
		inputMint, outputMint := "USDC-fixture", "TSLAx-fixture"
		in, out, minOut := "100000000", "500", "495"
		observed, feeBPS, fixedFee := buyAt, 20, "0.10"
		if input["input_mint"] == "TSLAx-fixture" {
			inputMint, outputMint = outputMint, inputMint
			in, out, minOut = "500", "110000000", "109000000"
			observed, feeBPS, fixedFee = sellAt, 30, "0.20"
		}
		if !reflect.DeepEqual(input, map[string]string{
			"paper_quote_fixture_schema_version": "0.1", "input_mint": inputMint,
			"output_mint": outputMint, "in_amount": in,
		}) {
			t.Errorf("collector sent a different fixture request: %+v", input)
			http.Error(w, "unexpected fixture request", http.StatusBadRequest)
			return
		}
		if mismatch.Load() {
			outputMint = "unrequested-token-fixture"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"paper_quote_fixture_schema_version": "0.1", "input_mint": inputMint,
			"output_mint": outputMint, "in_amount": in, "out_amount": out, "min_out_amount": minOut,
			"observed_at": observed, "available_at": observed.Add(time.Millisecond), "expires_at": f.expiry,
			"fee_bps": feeBPS, "fixed_fee_usd": fixedFee,
		})
	}))
	t.Cleanup(source.Close)
	requestPath := saveDocument(t, f.dir, "quote-collection-request", encode(t, map[string]any{
		"paper_quote_collection_schema_version": "0.1", "account_id": f.accountID,
		"identity_registry_sha256": registry.SHA256, "cash_mint": "USDC-fixture",
		"requests": []map[string]any{
			{"security_id": f.securityID, "instrument": f.instrument, "token_mint": "TSLAx-fixture", "side": "BUY", "amount_raw": "100000000"},
			{"security_id": f.securityID, "instrument": f.instrument, "token_mint": "TSLAx-fixture", "side": "SELL", "amount_raw": "500"},
		},
	}))
	collect := func(path string) ([]byte, error) {
		cmd := exec.CommandContext(f.ctx, filepath.Join(bin, "paper-quote-collect"+extension),
			"--request", requestPath, "--source", source.URL, "--out", path, "--as-of", cutoff.Format(time.RFC3339Nano))
		return cmd.CombinedOutput()
	}
	connection, err := pgx.Connect(f.ctx, f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(f.ctx)
	checkCounts := func(trades, events int) {
		t.Helper()
		var state [6]int
		err := connection.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM research_jobs),
			(SELECT count(*) FROM paper_orders), (SELECT count(*) FROM paper_fills),
			(SELECT count(*) FROM research_paper_dispatch), (SELECT count(*) FROM paper_applications),
			(SELECT count(*) FROM paper_events)`).
			Scan(&state[0], &state[1], &state[2], &state[3], &state[4], &state[5])
		if err != nil {
			t.Fatal(err)
		}
		if want := [6]int{trades, trades, trades, trades, trades, events}; state != want {
			t.Fatalf("PostgreSQL job/order/fill/dispatch/application/event counts = %v, want %v", state, want)
		}
	}
	mismatch.Store(true)
	failedPath := filepath.Join(f.dir, "rejected-collected-tape.json")
	if output, err := collect(failedPath); err == nil {
		t.Fatalf("collector accepted an unrelated token: %s", output)
	}
	if _, err := os.Stat(failedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected source published an executable tape: %v", err)
	}
	config := f.config
	owner := *config.Identity
	owner.QuoteTape = paper_workflow.ArchiveReference{Ref: filepath.Base(failedPath), SHA256: strings.Repeat("f", 64)}
	config.Identity = &owner
	if _, err := f.invoke(config, f.portfolio, f.projected, registry.SHA256); err == nil {
		t.Fatalf("failed collection could create an entry or fill: %v", err)
	}
	checkCounts(0, 1)
	mismatch.Store(false)
	tapePath := filepath.Join(f.dir, "collected-tape.json")
	output, err := collect(tapePath)
	if err != nil {
		t.Fatalf("collect the local fixture quote: %v\n%s", err, output)
	}
	rawTape, err := os.ReadFile(tapePath)
	if err != nil {
		t.Fatal(err)
	}
	var collected struct {
		SHA256 string `json:"tape_sha256"`
	}
	if json.Unmarshal(output, &collected) != nil || collected.SHA256 != digestHex(rawTape) || calls.Load() != 3 {
		t.Fatalf("collection did not pin the exact HTTP-derived tape: %s", output)
	}
	if _, err := collect(tapePath); err == nil {
		t.Fatal("collection overwrote an existing immutable tape")
	}
	if unchanged, err := os.ReadFile(tapePath); err != nil || !bytes.Equal(unchanged, rawTape) {
		t.Fatalf("repeat collection changed the published tape: %v", err)
	}
	owner.QuoteTape = paper_workflow.ArchiveReference{Ref: filepath.Base(tapePath), SHA256: collected.SHA256}
	entry, err := f.invoke(config, f.portfolio, f.projected, registry.SHA256)
	if err != nil || !entry.Created || entry.Status != "queued" {
		t.Fatalf("collected tape could not enroll its owner-bound entry: %+v %v", entry, err)
	}
	quoteRoot := t.TempDir()
	pin := func(jobID, wantStatus string) {
		cmd := exec.CommandContext(f.ctx, filepath.Join(bin, "paper-quote-pin"+extension),
			"--account", f.accountID, "--tape", tapePath, "--sha256", collected.SHA256, "--quote-root", quoteRoot)
		cmd.Env = append(os.Environ(), "TRADINGBOT_DATABASE_URL="+f.dsn)
		output, err := cmd.CombinedOutput()
		var receipt struct {
			JobID  string `json:"job_id"`
			Status string `json:"status"`
		}
		if err != nil || json.Unmarshal(output, &receipt) != nil || receipt.JobID != jobID || receipt.Status != wantStatus {
			t.Fatalf("quote publisher did not return %s: %v\n%s", wantStatus, err, output)
		}
	}
	policyPath := saveDocument(t, f.dir, "collected-quote-policy", encode(t, config.Context.Policy))
	dispatch := func() []byte {
		var stdout bytes.Buffer
		err := run(f.ctx, f.dsn, []string{"dispatch", "--account", f.accountID, "--policy", policyPath,
			"--quote-root", quoteRoot, "--archive-root", f.dir, "--identity-registry-sha256", registry.SHA256,
			"--identity-quote-tape-sha256", collected.SHA256}, &stdout, f.clock)
		if err != nil {
			t.Fatalf("dispatch the collected quote: %v", err)
		}
		return stdout.Bytes()
	}
	buy := collectedFixtureLeg{side: "BUY", gross: "100", fixedFee: "0.10", fee: "0.3", bps: 20, observed: buyAt}
	completeCollectedQuoteResearch(t, f, entry.JobID, buy)
	purchase := applyCollectedQuote(t, f, entry.JobID, quoteRoot, buy, pin, dispatch)
	account, err := f.paper.GetAccount(f.ctx, f.accountID)
	if err != nil || account.Version != 3 || account.CashUSD != "899.7" || account.ReservedCashUSD != "0" ||
		len(account.Positions) != 1 || account.Positions[0].QuantityRaw != "500" {
		t.Fatalf("collected quote did not reconcile exact PostgreSQL balances: %+v %v", account, err)
	}
	checkCounts(1, 3)
	review := collectedSaleSchedule(t, f, owner, account, purchase.OrderID)
	reviewID := review(true)
	sell := collectedFixtureLeg{side: "SELL", gross: "110", fixedFee: "0.20", fee: "0.53", bps: 30, observed: sellAt}
	completeCollectedQuoteResearch(t, f, reviewID, sell)
	sale := applyCollectedQuote(t, f, reviewID, quoteRoot, sell, pin, dispatch)
	final, err := f.paper.GetAccount(f.ctx, f.accountID)
	if err != nil || final.Version != 5 || final.CashUSD != "1009.17" || final.ReservedCashUSD != "0" || len(final.Positions) != 0 {
		t.Fatalf("BUY → SELL did not close and reconcile the account: %+v %v", final, err)
	}
	checkCounts(2, 5)
	checkCollectedRoundTripPnL(t, f, purchase, sale)
	if again, err := f.invoke(config, f.portfolio, f.projected, registry.SHA256); err != nil || again.Created ||
		again.Status != "completed" || again.JobID != entry.JobID {
		t.Fatalf("repeat entry did not acknowledge the original job: %+v %v", again, err)
	}
	if replayedJob := review(false); replayedJob != reviewID {
		t.Fatalf("repeat review changed the job ID: %s instead of %s", replayedJob, reviewID)
	}
	if output := dispatch(); len(output) != 0 {
		t.Fatalf("repeat dispatch emitted another execution: %s", output)
	}
	checkCounts(2, 5)
	if replay, err := f.paper.GetAccount(f.ctx, f.accountID); err != nil || !reflect.DeepEqual(replay, final) {
		t.Fatalf("repeat dispatch changed the closed account: %+v %v", replay, err)
	}
	checkCollectedRoundTripPnL(t, f, purchase, sale)
	t.Logf("BUY fee %s, SELL fee %s, final cash %s USD; 2 fills after replay",
		purchase.Fills[0].FeeUSD, sale.Fills[0].FeeUSD, final.CashUSD)
}

type collectedFixtureLeg struct {
	side, gross, fixedFee, fee string
	bps                        int
	observed                   time.Time
}

func applyCollectedQuote(t *testing.T, f stableEntryFixture, jobID, quoteRoot string, leg collectedFixtureLeg,
	pin func(string, string), dispatch func() []byte) ledger.Order {
	t.Helper()
	result, err := f.research.GetResult(f.ctx, jobID)
	if err != nil || !leg.observed.After(result.CompletedAt) {
		t.Fatalf("%s replay quote must follow the durable research receipt: %v", leg.side, err)
	}
	if delay := leg.observed.Add(time.Millisecond).Sub(f.clock()); delay >= 0 {
		time.Sleep(delay + 20*time.Millisecond)
	}
	pin(jobID, "pinned")
	pin(jobID, "replayed")
	rawQuote, err := localQuoteSource(quoteRoot)(f.ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	registry := f.config.Identity.Registry.SHA256
	quote, err := paper_sim.ParseQuote(rawQuote)
	if err != nil || quote.Source != paper_sim.Source || quote.Side != leg.side || quote.QuantityRaw != "500" ||
		quote.GrossUSD != leg.gross || quote.FixedFeeUSD != leg.fixedFee || quote.FeeBPS == nil || *quote.FeeBPS != leg.bps ||
		quote.SecurityID != f.securityID || quote.Instrument != f.instrument || quote.IdentityRegistrySHA256 != registry ||
		!quote.ObservedAt.Equal(leg.observed) || !quote.ExpiresAt.Equal(f.expiry) {
		t.Fatalf("%s quote normalization lost exact amounts or identity: %+v %v", leg.side, quote, err)
	}
	var applied paper_workflow.FollowResult
	if output := dispatch(); json.Unmarshal(output, &applied) != nil || applied.Status != "applied" ||
		applied.Application == nil || applied.Application.Execution == nil {
		t.Fatalf("HTTP-collected %s did not produce a durable fill: %s", leg.side, output)
	}
	execution := applied.Application.Execution
	if execution.QuoteSHA256 != digestHex(rawQuote) || execution.SecurityID != f.securityID ||
		execution.IdentityRegistrySHA256 != registry {
		t.Fatalf("%s dispatch lost the HTTP-collected quote provenance: %+v", leg.side, execution)
	}
	order := execution.Order
	if order.Status != "filled" || order.Side != leg.side || order.Instrument != f.instrument || order.QuantityRaw != "500" ||
		len(order.Fills) != 1 || order.Fills[0].QuantityRaw != "500" || order.Fills[0].GrossUSD != leg.gross || order.Fills[0].FeeUSD != leg.fee {
		t.Fatalf("collected %s booked a different economic result: %+v", leg.side, order)
	}
	return order
}

func collectedSaleSchedule(t *testing.T, f stableEntryFixture, owner paper_workflow.OwnerIdentity, account ledger.Account, openingOrderID string) func(bool) string {
	t.Helper()
	asOf := f.clock()
	snapshot := f.config.Context
	snapshot.SnapshotVersion = account.Version
	snapshot.Marks = []paper_risk.Mark{{Instrument: f.instrument, PriceUSD: "22", At: asOf, AvailableAt: asOf}}
	portfolio, err := paper_workflow.Snapshot(account, snapshot, asOf)
	if err != nil {
		t.Fatal(err)
	}
	event := research_scheduler.EventReference{Kind: "news", Provider: "fixture-market", RecordID: "collected-sale-catalyst"}
	archive := encode(t, map[string]any{
		"schema_version": "0.1", "source_mode": paper_sim.Source,
		"records": []map[string]any{{
			"kind": event.Kind, "provider": event.Provider, "record_id": event.RecordID, "symbol": "TSLA", "revision": 1,
			"event_time": asOf, "available_at": asOf, "ingested_at": asOf, "payload": map[string]string{"security_id": f.securityID},
		}},
	})
	manifest := paper_workflow.ArchiveReference{Ref: filepath.Base(saveDocument(t, f.dir, "collected-sale-archive", archive)), SHA256: digestHex(archive)}
	config := paper_thesis.ScheduleRequest{
		Version: paper_workflow.IdentityVersion, OpeningOrderID: openingOrderID,
		Context: snapshot, Portfolio: &portfolio, Manifest: manifest, Budget: 3, Event: &event,
		Identity: &paper_thesis.ReviewIdentity{OwnerIdentity: owner, ProjectedSnapshotSHA256: f.projected},
	}
	path := saveDocument(t, f.dir, "collected-sale-schedule", encode(t, config))
	return func(created bool) string {
		t.Helper()
		var output bytes.Buffer
		err := run(f.ctx, f.dsn, []string{"schedule-review", "--config", path, "--archive-root", f.dir,
			"--identity-registry-sha256", owner.Registry.SHA256}, &output, f.clock)
		var receipt paper_thesis.ScheduleReceipt
		status := "completed"
		if created {
			status = "queued"
		}
		if err != nil || json.Unmarshal(output.Bytes(), &receipt) != nil || receipt.Created != created ||
			receipt.Status != status || receipt.JobID == "" {
			t.Fatalf("schedule collected sale with status %s: %v\n%s", status, err, output.Bytes())
		}
		return receipt.JobID
	}
}

func checkCollectedRoundTripPnL(t *testing.T, f stableEntryFixture, purchase, sale ledger.Order) {
	t.Helper()
	book, err := paper_memory.Load(f.ctx, f.paper, f.accountID, f.clock())
	if err != nil || book.AccountVersion != 5 || book.CashUSD != "1009.17" ||
		book.RecordedFeesUSD != "0.83" || book.RealizedNetPnLUSD != "9.17" ||
		len(book.OpenLots) != 0 || len(book.Realized) != 1 {
		t.Fatalf("PostgreSQL FIFO result does not reconcile BUY → SELL: %+v %v", book, err)
	}
	closed := book.Realized[0]
	if closed.FillEventID != sale.Fills[0].EventID || closed.CostBasisUSD != "100.3" ||
		len(closed.Lots) != 1 || closed.Lots[0].BuyFillEventID != purchase.Fills[0].EventID ||
		closed.Lots[0].QuantityRaw != "500" {
		t.Fatalf("sale did not close the exact lot opened by the purchase: %+v", closed)
	}
}

func completeCollectedQuoteResearch(t *testing.T, f stableEntryFixture, jobID string, leg collectedFixtureLeg) {
	t.Helper()
	task, err := f.research.GetPaperDispatch(f.ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	var application paper_workflow.Request
	if err := contract.Decode(task.Request, contract.PaperApplication, &application); err != nil {
		t.Fatal(err)
	}
	identity := application.Identity
	idPrefix := "80000000"
	if leg.side == "SELL" {
		idPrefix = "81000000"
	}
	id := func(suffix string) string { return idPrefix + "-0000-4000-8000-00000000000" + suffix }
	lease, err := f.research.ClaimWithIdentityRegistry(f.ctx, "collected-quote-fixture",
		id("1"), time.Hour, identity.Registry.SHA256)
	if err != nil || lease.Job.ID != jobID {
		t.Fatalf("claim collected quote research: %+v %v", lease.Job, err)
	}
	var request struct {
		Portfolio contract.Portfolio              `json:"portfolio"`
		Manifest  paper_workflow.ArchiveReference `json:"data_manifest"`
	}
	if err := json.Unmarshal(lease.Job.Request, &request); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.research.AuthorizeCall(f.ctx, jobID, lease.WorkerID, lease.Token, id("2")); err != nil {
		t.Fatal(err)
	}
	api, err := control.NewHandlerWithResearchIdentity(time.Hour, f.clock, f.research, f.paper, identity.Registry.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	evidence := "Synthetic " + leg.side + " decision for the local HTTP quote collection test"
	symbol := identity.Securities[0].Symbol
	evidenceID := symbol + ":final_trade_decision"
	done := f.clock()
	post(t, api, "/v1/research/jobs/"+jobID+"/complete", map[string]any{
		"result_schema_version": "0.3", "worker_id": lease.WorkerID, "lease_token": lease.Token,
		"batch": contract.Batch{Version: contract.BatchVersion, Mode: "research",
			RunID: id("3"), SnapshotID: request.Portfolio.ID, AsOf: request.Portfolio.AsOf,
			Created: done, Expires: done.Add(time.Minute), Evidence: map[string]string{evidenceID: digestHex([]byte(evidence))},
			Intents: []contract.Intent{{ID: id("4"), Symbol: symbol, Action: leg.side,
				Notional: leg.gross, Confidence: new(0.75), Expected: new(2.0), CostBPS: 50,
				Horizon: contract.Horizon{Min: 1, Max: 2}, Thesis: []string{"synthetic fixture"},
				Invalidation: []string{"synthetic fixture"}, Evidence: []string{evidenceID}}}},
		"reports": map[string]map[string]string{symbol: {"final_trade_decision": evidence}},
		"metadata": map[string]any{
			"source_mode": "tradingagents_offline", "alpha_measured": false, "intraday_pit_verified": false,
			"llm_calls": 1, "input_tokens": 0, "output_tokens": 0, "missing_usage_calls": 1, "usd_cost": nil,
			"provenance": map[string]any{"job_id": jobID, "attempt": lease.Attempt,
				"policy_version": application.SnapshotContext.Policy.Version, "data_manifest": request.Manifest,
				"worker_version": "fixture", "upstream_version": "fixture", "upstream_sha": strings.Repeat("a", 40),
				"config_sha256": strings.Repeat("a", 64), "model_id": "scripted-offline-v1",
				"identity_registry": identity.Registry, "projected_snapshot_sha256": identity.ProjectedSnapshotSHA256,
				"security_bindings": []paper_workflow.ResearchSecurityBinding{{Symbol: symbol, SecurityID: f.securityID}},
			},
		},
	})
}
