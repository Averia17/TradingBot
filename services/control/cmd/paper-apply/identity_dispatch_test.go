package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/control"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paperapply"
	"tradingbot/control/internal/paperquotes"
	"tradingbot/control/internal/papersim"
)

func TestIdentityDispatchRejectsUnapprovedBytesAndRecoversBoundReservation(t *testing.T) {
	if os.Getenv("TRADINGBOT_TEST_DATABASE_URL") == "" {
		t.Skip("TRADINGBOT_TEST_DATABASE_URL is required for real PostgreSQL tests")
	}
	publisher := filepath.Join(t.TempDir(), "paper-quote-pin.exe")
	build := exec.CommandContext(t.Context(), "go", "build", "-mod=readonly", "-o", publisher, "../paper-quote-pin")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build paper quote publisher: %v\n%s", err, output)
	}
	for _, outcome := range []string{"fill", "timeout"} {
		t.Run(outcome, func(t *testing.T) { checkIdentityDispatch(t, publisher, outcome) })
	}
}

func checkIdentityDispatch(t *testing.T, publisher, outcome string) {
	var quote, wrongIssuer papersim.Quote
	var availableAt time.Time
	fixture := newStableEntryFixture(t, func(at time.Time, instrument ledger.Instrument, securityID, registrySHA string) []any {
		quote = papersim.Quote{
			Version: papersim.IdentityQuoteVersion, Source: papersim.Source, Instrument: instrument,
			SecurityID: securityID, IdentityRegistrySHA256: registrySHA, Side: "BUY", QuantityRaw: "500",
			GrossUSD: "100", FeeBPS: new(20), FixedFeeUSD: "0", ObservedAt: at.Add(7 * time.Second),
			ExpiresAt: at.Add(time.Minute), Execution: outcome,
		}
		availableAt = quote.ObservedAt.Add(time.Millisecond)
		wrongIssuer = quote
		wrongIssuer.SecurityID = "previous-equity-id"
		return []any{
			map[string]any{"available_at": availableAt, "firmness": "firm", "quote": wrongIssuer},
			map[string]any{"available_at": availableAt, "firmness": "firm", "quote": quote},
		}
	})
	entry := fixture.queue(t)
	task, err := fixture.research.GetPaperDispatch(fixture.ctx, entry.JobID)
	if err != nil {
		t.Fatal(err)
	}
	var application paperapply.Request
	if err := contract.Decode(task.Request, contract.PaperApplication, &application); err != nil {
		t.Fatal(err)
	}
	identity := application.Identity
	if identity == nil || len(identity.Securities) != 1 {
		t.Fatalf("missing owner identity: %+v", identity)
	}
	lease, err := fixture.research.ClaimWithIdentityRegistry(fixture.ctx, "bound-fill-fixture",
		"50000000-0000-4000-8000-000000000001", time.Hour, identity.Registry.SHA256)
	if err != nil || lease.Job.ID != entry.JobID {
		t.Fatalf("claim scheduled identity research: %+v %v", lease, err)
	}
	_, _, err = fixture.research.AuthorizeCall(fixture.ctx, entry.JobID, lease.WorkerID, lease.Token,
		"50000000-0000-4000-8000-000000000002")
	if err != nil {
		t.Fatal(err)
	}
	handler, err := control.NewHandlerWithResearchIdentity(time.Hour, fixture.clock, fixture.research, fixture.paper, identity.Registry.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	symbol := identity.Securities[0].Symbol
	const evidenceText = "Synthetic acceptance fixture for an owner-bound BUY"
	evidenceID := symbol + ":final_trade_decision"
	done := fixture.clock()
	post(t, handler, "/v1/research/jobs/"+entry.JobID+"/complete", map[string]any{
		"result_schema_version": "0.3", "worker_id": lease.WorkerID, "lease_token": lease.Token,
		"batch": contract.Batch{
			Version: contract.BatchVersion, Mode: "research", RunID: "50000000-0000-4000-8000-000000000003",
			SnapshotID: fixture.portfolio.ID, AsOf: fixture.portfolio.AsOf, Created: done,
			Expires:  done.Add(time.Minute),
			Evidence: map[string]string{evidenceID: digestHex([]byte(evidenceText))},
			Intents: []contract.Intent{{
				ID: "50000000-0000-4000-8000-000000000004", Symbol: symbol, Action: "BUY", Notional: "100",
				Confidence: new(0.75), Expected: new(2.0), CostBPS: 30,
				Horizon: contract.Horizon{Min: 1, Max: 2}, Thesis: []string{"synthetic fixture"},
				Invalidation: []string{"synthetic fixture"}, Evidence: []string{evidenceID},
			}},
		},
		"reports": map[string]map[string]string{symbol: {"final_trade_decision": evidenceText}},
		"metadata": map[string]any{
			"source_mode": "tradingagents_offline", "alpha_measured": false, "intraday_pit_verified": false,
			"llm_calls": 1, "input_tokens": 0, "output_tokens": 0, "missing_usage_calls": 1, "usd_cost": nil,
			"provenance": map[string]any{
				"job_id": entry.JobID, "attempt": lease.Attempt, "policy_version": application.SnapshotContext.Policy.Version,
				"data_manifest": fixture.manifest, "worker_version": "fixture", "upstream_version": "fixture",
				"upstream_sha": strings.Repeat("a", 40), "config_sha256": strings.Repeat("a", 64), "model_id": "scripted-offline-v1",
				"identity_registry": identity.Registry, "projected_snapshot_sha256": identity.ProjectedSnapshotSHA256,
				"security_bindings": []paperapply.ResearchSecurityBinding{{Symbol: symbol, SecurityID: quote.SecurityID}},
			},
		},
	})
	result, err := fixture.research.GetResult(fixture.ctx, entry.JobID)
	if err != nil || !quote.ObservedAt.After(result.CompletedAt) {
		t.Fatalf("fixture quote must follow the durable completion: %+v %v", result, err)
	}
	if delay := availableAt.Sub(fixture.clock()); delay >= 0 {
		time.Sleep(delay + 20*time.Millisecond)
	}
	options := boundDispatchOptions{fixture.dir, identity.Registry.SHA256, identity.QuoteTape.SHA256}
	pin := func(raw []byte) {
		saveDocument(t, fixture.dir, "identity-pinned-quote", raw)
		saveDocument(t, fixture.dir, entry.JobID, encode(t, map[string]any{
			"paper_quote_pin_schema_version": "0.1", "job_id": entry.JobID,
			"quote": map[string]string{"ref": "identity-pinned-quote.json", "sha256": digestHex(raw)},
		}))
	}
	counterfeit := quote
	counterfeit.GrossUSD = "101"
	pin(encode(t, counterfeit))
	quotes := localQuoteSource(fixture.dir)
	policy := application.SnapshotContext.Policy
	if _, err := dispatchTask(fixture.ctx, fixture.research, fixture.paper, task, policy, quotes, options); !errors.Is(err, paperquotes.ErrQuoteNotInTape) {
		t.Fatalf("mutable job pin bypassed owner tape: %v", err)
	}
	pin(encode(t, wrongIssuer))
	if _, err := dispatchTask(fixture.ctx, fixture.research, fixture.paper, task, policy, quotes, options); !errors.Is(err, paperapply.ErrInvalid) {
		t.Fatalf("owner tape quote for another security was accepted: %v", err)
	}
	if _, err := fixture.paper.GetApplication(fixture.ctx, entry.JobID); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("a rejected quote left paper state: %v", err)
	}
	expired, err := paperapply.FollowBound(fixture.ctx, fixture.research, fixture.paper,
		application, nil, nil, fixture.expiry)
	if err != nil || expired.Status != "expired" {
		t.Fatalf("expired registry allowed a new reservation pass: %+v %v", expired, err)
	}
	raw := encode(t, quote)
	jobPin := filepath.Join(fixture.dir, entry.JobID+".json")
	if err := os.Remove(jobPin); err != nil {
		t.Fatal(err)
	}
	publish := exec.CommandContext(fixture.ctx, publisher, "--account", fixture.accountID,
		"--tape", filepath.Join(fixture.dir, identity.QuoteTape.Ref),
		"--sha256", identity.QuoteTape.SHA256, "--quote-root", fixture.dir)
	publish.Env = append(os.Environ(), "TRADINGBOT_DATABASE_URL="+fixture.dsn)
	if output, err := publish.CombinedOutput(); err != nil {
		t.Fatalf("publish owner-bound quote: %v\n%s", err, output)
	}
	if pinned, err := quotes(fixture.ctx, entry.JobID); err != nil || !bytes.Equal(pinned, raw) {
		t.Fatalf("publisher chose bytes outside approved security: %s %v", pinned, err)
	}
	interrupted := errors.New("fixture dispatcher lost after reservation")
	if _, err := paperapply.FollowBound(fixture.ctx, fixture.research,
		interruptedFollowStore{Store: fixture.paper, err: interrupted},
		application, quotes, options.verifier(application), fixture.clock()); !errors.Is(err, interrupted) {
		t.Fatalf("could not interrupt after the owner-bound reserve: %v", err)
	}
	reserved, err := fixture.paper.GetApplication(fixture.ctx, entry.JobID)
	if err != nil || reserved.OrderID == "" || !bytes.Equal(reserved.Quote, raw) {
		t.Fatalf("reserve lost exact quote: %+v %v", reserved, err)
	}
	if err := os.Remove(jobPin); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(fixture.dir, identity.QuoteTape.Ref)); err != nil {
		t.Fatal(err)
	}
	rotated := policy
	rotated.Version = "rotated-fixture"
	replay, err := dispatchTask(fixture.ctx, fixture.research, fixture.paper, task, rotated, nil, boundDispatchOptions{})
	if err != nil || replay.Version != paperapply.IdentityVersion || replay.Status != "applied" ||
		replay.Application == nil || replay.Application.Execution == nil {
		t.Fatalf("identity recovery failed: %+v %v", replay, err)
	}
	execution := replay.Application.Execution
	if execution.ReservationRecorded || execution.Order.OrderID != reserved.OrderID ||
		execution.SecurityID != quote.SecurityID || execution.IdentityRegistrySHA256 != identity.Registry.SHA256 ||
		execution.QuoteSHA256 != digestHex(raw) || execution.Order.Instrument != quote.Instrument {
		t.Fatalf("recovered receipt changed identity or duplicated reservation: %+v", execution)
	}
	wantVersion := int64(3)
	if outcome == "timeout" {
		reconcileIdentityTimeout(t, fixture, execution.Order)
		wantVersion++
	} else if execution.Order.Status != "filled" {
		t.Fatalf("bound simulated fill has status %s", execution.Order.Status)
	}
	account, err := fixture.paper.GetAccount(fixture.ctx, fixture.accountID)
	if err != nil || account.Version != wantVersion || account.CashUSD != "899.8" || account.ReservedCashUSD != "0" ||
		len(account.Positions) != 1 || account.Positions[0].Instrument != quote.Instrument ||
		account.Positions[0].QuantityRaw != "500" {
		t.Fatalf("recovered fill changed the economic accounting: %+v %v", account, err)
	}
	order, err := fixture.paper.GetOrder(fixture.ctx, fixture.accountID, execution.Order.OrderID)
	if err != nil {
		t.Fatal(err)
	}
	opening, err := paperapply.ReadOpeningIdentity(fixture.ctx, fixture.research, fixture.paper, order, fixture.clock())
	if err != nil || opening.SecurityID != quote.SecurityID || opening.Symbol != symbol ||
		opening.Instrument != order.Instrument || opening.Registry != identity.Registry ||
		opening.Decision.ID != order.DecisionID || len(opening.ApplicationSHA256) != 64 {
		t.Fatalf("opening lost the durable security proof after quote rotation: %+v %v", opening, err)
	}
	edited := order
	edited.Instrument.RegistryVersion = "different-issuer-registry"
	if _, err := paperapply.ReadOpeningIdentity(fixture.ctx, fixture.research, fixture.paper, edited, fixture.clock()); err == nil {
		t.Fatal("edited held tuple inherited the original opening identity")
	}
	if outcome == "fill" {
		checkIdentityReviewPreparer(t, fixture, order)
		checkStableHorizonOutcome(t, fixture, order)
		checkStableThesisReview(t, fixture, order)
		checkReviewCycleRestart(t, fixture, order)
	} else {
		checkUnboundHorizonSale(t, fixture, order)
	}
}

func reconcileIdentityTimeout(t *testing.T, fixture stableEntryFixture, order ledger.Order) {
	t.Helper()
	account, err := fixture.paper.GetAccount(fixture.ctx, fixture.accountID)
	if err != nil || order.Status != "unknown" || account.ReservedCashUSD != "100.2" || account.CashUSD != "1000" {
		t.Fatalf("unknown outcome lost its original reserve: %+v %v", account, err)
	}
	now := fixture.clock()
	observation := ledger.ReconciliationObservation{
		Version: ledger.ReconciliationVersion, Source: ledger.ReconciliationSource,
		AccountID: order.AccountID, OrderID: order.OrderID, QuoteID: order.QuoteID,
		ObservationID: "50000000-0000-4000-8000-000000000005", Outcome: "filled",
		ObservedAt: now, ReceivedAt: now, QuantityRaw: "500", GrossUSD: "100", FeeUSD: "0.2",
	}
	runObservation := func(observation ledger.ReconciliationObservation) (reconciliationReceipt, error) {
		raw := encode(t, observation)
		path := saveDocument(t, fixture.dir, "identity-observation", raw)
		var stdout bytes.Buffer
		err := run(fixture.ctx, fixture.dsn, []string{"reconcile", "--observation", path, "--sha256", digestHex(raw)}, &stdout, fixture.clock)
		if err != nil {
			return reconciliationReceipt{}, err
		}
		var receipt reconciliationReceipt
		return receipt, json.Unmarshal(stdout.Bytes(), &receipt)
	}
	wrong := observation
	wrong.QuoteID = "50000000-0000-4000-8000-000000000006"
	if _, err := runObservation(wrong); !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("observation for another quote was accepted: %v", err)
	}
	for _, recorded := range []bool{true, false} {
		receipt, err := runObservation(observation)
		if err != nil || receipt.Recorded != recorded || receipt.Order.Status != "filled" ||
			receipt.Order.Instrument != order.Instrument || receipt.Order.QuoteID != order.QuoteID {
			t.Fatalf("bound timeout reconciliation changed its original order: %+v %v", receipt, err)
		}
	}
}

func digestHex(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}
