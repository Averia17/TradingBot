package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/control"
	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paper_risk"
	"tradingbot/control/internal/paper_sim"
	"tradingbot/control/internal/paper_thesis"
	"tradingbot/control/internal/paper_workflow"
	"tradingbot/control/internal/research_scheduler"
	"tradingbot/control/internal/research_worker"
)

const scheduledJupiterAccount = "11111111-1111-4111-8111-111111111111"

func TestScheduledPaperJupiterRehearsal(t *testing.T) {
	ctx, dsn, database := scheduledJupiterDatabase(t)
	research, paper, err := openPaperStores(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(research.Close)
	t.Cleanup(paper.Close)
	controlHandler := control.NewHandlerWithPaper(time.Hour, time.Now, research, paper)
	post(t, controlHandler, "/v1/paper/accounts", ledger.CreateAccount{ID: scheduledJupiterAccount, InitialCashUSD: "1000"})
	controlServer := httptest.NewServer(controlHandler)
	t.Cleanup(controlServer.Close)
	fixtureURL := startScheduledJupiterFixture(t, ctx, dsn)

	dir := t.TempDir()
	instrument := ledger.Instrument{ID: "fixture:Jupiter", RegistryVersion: "fixture-v1", Decimals: 0}
	policy := paper_risk.Policy{
		Version: "scheduled-jupiter-fixture", MaxOrderUSD: "300", MinFreeCashUSD: "100",
		MaxPositionBPS: 5000, MaxGroupBPS: 5000, MaxGrossBPS: 9000, MaxFeeBPS: 30, MaxMarkAgeSeconds: 120,
		Instruments: []paper_risk.InstrumentRule{{Instrument: instrument, Symbol: "TSLA", Group: "fixture"}},
	}
	policyPath := saveDocument(t, dir, "owner-policy", encode(t, policy))
	ordinaryDispatchArgs := []string{"dispatch", "--account", scheduledJupiterAccount, "--policy", policyPath, "--quote-root", dir}
	dispatchArgs := append(ordinaryDispatchArgs, "--jupiter-fixture-url", fixtureURL)
	dispatch := func() []byte {
		t.Helper()
		var output bytes.Buffer
		if err := run(ctx, dsn, dispatchArgs, &output, time.Now); err != nil {
			t.Fatalf("dispatch scheduled paper jobs: %v", err)
		}
		return output.Bytes()
	}
	account := func() ledger.Account {
		t.Helper()
		state, err := paper.GetAccount(ctx, scheduledJupiterAccount)
		if err != nil {
			t.Fatal(err)
		}
		return state
	}
	snapshot := func(price string, at time.Time) paper_workflow.SnapshotContext {
		return paper_workflow.SnapshotContext{AccountID: scheduledJupiterAccount, Policy: policy,
			Marks: []paper_risk.Mark{{Instrument: instrument, PriceUSD: price, At: at, AvailableAt: at}}}
	}
	clock := func() time.Time {
		t.Helper()
		var now time.Time
		if err := database.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
			t.Fatal("read isolated database clock")
		}
		return now.UTC()
	}

	initial := account()
	at := clock()
	opening := snapshot("2", at)
	opening.SnapshotVersion = initial.Version
	portfolio, err := paper_workflow.Snapshot(initial, opening, at)
	if err != nil {
		t.Fatal(err)
	}
	archive := []byte(`{"schema_version":"0.1","source_mode":"synthetic_fixture","records":[]}`)
	saveDocument(t, dir, "entry-data", archive)
	selection := selectionFixture(t, "22222222-2222-4222-8222-222222222222", portfolio, policy.Version, sha256.Sum256(archive))
	selectionPath := saveDocument(t, dir, "entry-selection", selection)
	entry := paper_workflow.EntrySchedule{
		Version: paper_workflow.Version, ID: "scheduled-jupiter-entry", At: clock().Add(2 * time.Second),
		MaxLateness: 60, Budget: 20, Context: opening, SelectionSHA256: digestHex(selection),
	}
	entryPath := saveDocument(t, dir, "owner-entry", encode(t, entry))
	entryArgs := []string{"schedule-entry", "--config", entryPath, "--selection", selectionPath, "--archive-root", dir}
	if early := scheduledJupiterCommand[paper_workflow.EntryReceipt](t, ctx, dsn, entryArgs); early.Status != "not_due" || early.JobID != "" {
		t.Fatalf("future entry window admitted a job: %+v", early)
	}
	queued := scheduledJupiterCommand[paper_workflow.EntryReceipt](t, ctx, dsn, append(entryArgs, "--poll-interval", "1s"))
	if queued.Status != "queued" || !queued.Created || queued.JobID == "" {
		t.Fatalf("entry window did not admit exactly one job: %+v", queued)
	}
	if replayed := scheduledJupiterCommand[paper_workflow.EntryReceipt](t, ctx, dsn, entryArgs); replayed.Created || replayed.JobID != queued.JobID {
		t.Fatalf("early scheduler restart changed the durable job: %+v", replayed)
	}
	if output := dispatch(); len(output) != 0 {
		t.Fatalf("dispatcher processed the uncompleted job: %s", output)
	}

	complete := func(jobID, action, notional string) jobs.Result {
		t.Helper()
		return completeScheduledJupiter(t, ctx, research, controlServer.URL, jobID, action, notional)
	}
	buy := complete(queued.JobID, "BUY", "200")
	pinScheduledJupiterQuote(t, dir, buy, instrument, "BUY", "200", clock())
	requireScheduledJupiterDispatch(t, dispatch(), buy.JobID, "awaiting_recovery")
	pendingBuy := account()
	if pendingBuy.CashUSD != "1000" || pendingBuy.ReservedCashUSD != "200" || len(pendingBuy.Positions) != 0 {
		t.Fatalf("pending Jupiter buy did not keep the paper reserve: %+v", pendingBuy)
	}
	requireScheduledJupiterCounts(t, ctx, database, 1, 1, 0, 0)
	var fallback bytes.Buffer
	err = run(ctx, dsn, ordinaryDispatchArgs, &fallback, time.Now)
	if err == nil || !strings.Contains(err.Error(), "pinned settlement mode") || fallback.Len() != 0 {
		t.Fatalf("dispatcher without its fixture route accepted pending BUY: %s / %v", fallback.Bytes(), err)
	}
	requireScheduledJupiterCounts(t, ctx, database, 1, 1, 0, 0)
	bought := requireScheduledJupiterDispatch(t, dispatch(), buy.JobID, "applied")
	openingOrder := requireScheduledJupiterFill(t, bought, instrument, "BUY", "200", "200")
	if state := account(); state.CashUSD != "800" || state.ReservedCashUSD != "0" || len(state.Positions) != 1 ||
		state.Positions[0].QuantityRaw != "100" || state.Positions[0].ReservedQuantityRaw != "0" {
		t.Fatalf("buy finality did not establish the paper position: %+v", state)
	}

	reviewArgs := func(name string) (earlyArgs, dueArgs []string, ready time.Time) {
		t.Helper()
		now := clock()
		ready = now.Add(100 * time.Millisecond)
		reviewArchive := encode(t, map[string]any{"schema_version": "0.1", "source_mode": "synthetic_fixture", "records": []map[string]any{{
			"kind": "news", "provider": "local", "record_id": name, "symbol": "TSLA", "revision": 1,
			"event_time": ready, "available_at": ready, "ingested_at": ready, "payload": map[string]string{"headline": "Synthetic " + name},
		}}})
		saveDocument(t, dir, name+"-archive", reviewArchive)
		review := paper_thesis.ScheduleRequest{
			Version: paper_thesis.Version, OpeningOrderID: openingOrder.OrderID,
			Context: snapshot("2.1", now), Budget: 3,
			Manifest: research_scheduler.ArchiveReference{Ref: name + "-archive.json", SHA256: digestHex(reviewArchive)},
		}
		earlyPath := saveDocument(t, dir, name+"-not-due", encode(t, review))
		review.Event = &research_scheduler.EventReference{Kind: "news", Provider: "local", RecordID: name}
		duePath := saveDocument(t, dir, name+"-due", encode(t, review))
		return []string{"schedule-review", "--config", earlyPath, "--archive-root", dir},
			[]string{"schedule-review", "--config", duePath, "--archive-root", dir}, ready
	}
	scheduleReview := func(name string) ([]string, paper_thesis.ScheduleReceipt) {
		t.Helper()
		earlyArgs, args, ready := reviewArgs(name)
		if early := scheduledJupiterCommand[paper_thesis.ScheduleReceipt](t, ctx, dsn, earlyArgs); early.Status != "not_due" || early.JobID != "" {
			t.Fatalf("review before its trigger admitted a job: %+v", early)
		}
		waitScheduledJupiter(t, ctx, clock, ready)
		review := scheduledJupiterCommand[paper_thesis.ScheduleReceipt](t, ctx, dsn, args)
		if review.Status != "queued" || !review.Created || review.JobID == "" || review.Trigger == nil || review.Snapshot == nil {
			t.Fatalf("eligible review did not queue: %+v", review)
		}
		return args, review
	}
	holdArgs, hold := scheduleReview("jupiter-hold")
	complete(hold.JobID, "HOLD", "0")
	held := requireScheduledJupiterDispatch(t, dispatch(), hold.JobID, "hold")
	if held.Application == nil || held.Application.Execution != nil {
		t.Fatalf("HOLD was routed as an order: %+v", held)
	}
	requireScheduledJupiterCounts(t, ctx, database, 1, 1, 1, 2)
	if replay := scheduledJupiterCommand[paper_thesis.ScheduleReceipt](t, ctx, dsn, holdArgs); replay.Created ||
		replay.JobID != hold.JobID || replay.Status != "completed" {
		t.Fatalf("HOLD review replay did not preserve its result: %+v", replay)
	}

	sellArgs, sale := scheduleReview("jupiter-sell")
	if sale.JobID == hold.JobID || sale.JobID == buy.JobID {
		t.Fatalf("review cycles reused the job identity: %+v", sale)
	}
	sell := complete(sale.JobID, "SELL", "210")
	pinScheduledJupiterQuote(t, dir, sell, instrument, "SELL", "210", clock())
	var sold paper_workflow.FollowResult
	pendingSeen := false
	pollScheduledJupiter(t, ctx, dsn, dispatchArgs, func(raw []byte) bool {
		if pendingSeen {
			sold = requireScheduledJupiterDispatch(t, raw, sell.JobID, "applied")
			return true
		}
		requireScheduledJupiterDispatch(t, raw, sell.JobID, "awaiting_recovery")
		pendingSell := account()
		if pendingSell.CashUSD != "800" || pendingSell.ReservedCashUSD != "0" ||
			len(pendingSell.Positions) != 1 || pendingSell.Positions[0].QuantityRaw != "100" || pendingSell.Positions[0].ReservedQuantityRaw != "100" {
			t.Fatalf("pending Jupiter sell released tokens or credited proceeds: %+v", pendingSell)
		}
		requireScheduledJupiterCounts(t, ctx, database, 2, 2, 1, 2)
		pendingSeen = true
		return false
	})
	soldOrder := requireScheduledJupiterFill(t, sold, instrument, "SELL", "210", "215")
	final := account()
	if final.CashUSD != "1015" || final.ReservedCashUSD != "0" || len(final.Positions) != 0 {
		t.Fatalf("scheduled Jupiter round trip did not close and reconcile: %+v", final)
	}
	requireScheduledJupiterCounts(t, ctx, database, 2, 2, 2, 3)

	replayEntry := scheduledJupiterCommand[paper_workflow.EntryReceipt](t, ctx, dsn, entryArgs)
	if replayEntry.Created || replayEntry.JobID != buy.JobID || replayEntry.Status != "completed" {
		t.Fatalf("entry replay did not preserve its result: %+v", replayEntry)
	}
	if replay := scheduledJupiterCommand[paper_thesis.ScheduleReceipt](t, ctx, dsn, sellArgs); replay.Created ||
		replay.Status != "not_due" || replay.JobID != "" {
		t.Fatalf("review replay reopened the closed position: %+v", replay)
	}
	if output := dispatch(); len(output) != 0 {
		t.Fatalf("dispatcher restart repeated terminal orders: %s", output)
	}
	requireScheduledJupiterCounts(t, ctx, database, 2, 2, 2, 3)
	if state := account(); state.Version != final.Version || state.CashUSD != final.CashUSD || len(state.Positions) != 0 {
		t.Fatalf("replay changed the closed account: %+v", state)
	}
	requireScheduledJupiterEvidence(t, ctx, fixtureURL)
	if os.Getenv("TRADINGBOT_TEST_JUPITER_NOTIFICATIONS") == "1" {
		requireJupiterPaperNotifications(t, ctx, dsn, database, scheduledJupiterAccount,
			[2]string{openingOrder.Fills[0].EventID, soldOrder.Fills[0].EventID})
	}
	t.Log("scheduled BUY, HOLD and SELL completed through leased worker and Paper dispatcher; final cash 1015 USD and two durable fills")
}

var (
	providerNotificationSchema = regexp.MustCompile(`^provider_paper_[a-z0-9]+$`)
	providerNotificationUUID   = regexp.MustCompile(`^[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}$`)
)

func TestPreparedProviderPaperNotifications(t *testing.T) {
	dsn := os.Getenv("TRADINGBOT_TEST_PROVIDER_NOTIFICATION_DSN")
	account := os.Getenv("TRADINGBOT_TEST_PROVIDER_NOTIFICATION_ACCOUNT")
	rawFills := os.Getenv("TRADINGBOT_TEST_PROVIDER_NOTIFICATION_FILLS")
	if dsn == "" && account == "" && rawFills == "" {
		t.Skip("run with the exact ledger fills from an opt-in provider paper rehearsal")
	}
	if !validProviderNotificationDatabase(dsn) {
		t.Fatal("provider notification rehearsal requires an isolated loopback provider paper schema")
	}
	for _, name := range []string{"PGHOST", "PGHOSTADDR", "PGPORT", "PGDATABASE", "PGSERVICE", "PGSERVICEFILE", "PGOPTIONS", "PGTARGETSESSIONATTRS"} {
		if os.Getenv(name) != "" {
			t.Fatalf("clear %s before the isolated provider notification rehearsal", name)
		}
	}
	var fills []string
	if !providerNotificationUUID.MatchString(account) || json.Unmarshal([]byte(rawFills), &fills) != nil ||
		len(fills) != 2 || fills[0] == fills[1] || !providerNotificationUUID.MatchString(fills[0]) ||
		!providerNotificationUUID.MatchString(fills[1]) {
		t.Fatal("provider notification rehearsal requires the account and two distinct BUY/SELL fill UUIDs")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("connect to the prepared provider paper schema")
	}
	defer db.Close(ctx)
	requireJupiterPaperNotifications(t, ctx, dsn, db, account, [2]string(fills))
}

func validProviderNotificationDatabase(dsn string) bool {
	uri, err := url.Parse(dsn)
	if err != nil || (uri.Scheme != "postgresql" && uri.Scheme != "postgres") || uri.Opaque != "" ||
		uri.User == nil || uri.User.Username() == "" || uri.Fragment != "" || strings.Count(uri.Path, "/") != 1 || len(uri.Path) < 2 {
		return false
	}
	host, port, err := net.SplitHostPort(uri.Host)
	if err != nil || (host != "127.0.0.1" && host != "::1") {
		return false
	}
	portNumber, err := strconv.ParseUint(port, 10, 16)
	if err != nil || portNumber == 0 {
		return false
	}
	params, err := url.ParseQuery(uri.RawQuery)
	return err == nil && len(params) == 2 && len(params["sslmode"]) == 1 && params.Get("sslmode") == "disable" &&
		len(params["search_path"]) == 1 && providerNotificationSchema.MatchString(params.Get("search_path"))
}

func requireJupiterPaperNotifications(t *testing.T, ctx context.Context, dsn string, db *pgx.Conn, account string, fills [2]string) {
	t.Helper()
	events := make([]json.RawMessage, 2)
	for i, fill := range fills {
		var payload []byte
		err := db.QueryRow(ctx, `SELECT payload FROM paper_fill_outbox
			WHERE account_id=$1 AND event_id=$2 AND queued_at IS NULL`,
			account, fill).Scan(&payload)
		if err != nil {
			t.Fatalf("expected one committed, unrelayed paper fill for leg %d: %v", i+1, err)
		}
		events[i] = payload
	}
	command := exec.CommandContext(ctx, "go", "test", "-mod=readonly", "-count=1", "-v",
		"-run", "^TestJupiterPaperFillsReachTelegram$", "./internal/notifications")
	command.Dir = filepath.Join("..", "..", "..", "notifications")
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		name = strings.ToUpper(name)
		if !strings.HasPrefix(name, "TRADINGBOT_") && !strings.HasPrefix(name, "OPENAI_") &&
			!strings.HasPrefix(name, "PG") && name != "GOWORK" {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, "GOWORK=off", "TRADINGBOT_TEST_JUPITER_NOTIFICATION_DSN="+dsn,
		"TRADINGBOT_TEST_JUPITER_NOTIFICATION_EVENTS="+string(encode(t, events)))
	if brokers := os.Getenv("TRADINGBOT_TEST_KAFKA_BROKERS"); brokers != "" {
		command.Env = append(command.Env, "TRADINGBOT_TEST_KAFKA_BROKERS="+brokers)
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("deliver the exact paper fills: %v\n%s", err, output)
	}
	t.Logf("notification rehearsal:\n%s", output)

	var queued, tasks int
	err = db.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM paper_fill_outbox WHERE queued_at IS NOT NULL),
		(SELECT count(*) FROM pgqueue_tasks WHERE queue='paper-trade-filled-v1')`).Scan(&queued, &tasks)
	if err != nil || queued != 2 || tasks != 2 {
		t.Fatalf("paper notification outbox changed after relay: queued=%d tasks=%d / %v", queued, tasks, err)
	}
}

type scheduledJupiterWriter func([]byte) (int, error)

func (write scheduledJupiterWriter) Write(payload []byte) (int, error) { return write(payload) }

func pollScheduledJupiter(t *testing.T, ctx context.Context, dsn string, args []string, observe func([]byte) bool) {
	t.Helper()
	tickCtx, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	finished := false
	writer := scheduledJupiterWriter(func(payload []byte) (int, error) {
		finished = observe(payload)
		if finished {
			stop()
		}
		return len(payload), nil
	})
	err := run(tickCtx, dsn, append(args, "--poll-interval", "1s"), writer, time.Now)
	if err != nil || !finished {
		t.Fatalf("periodic paper dispatch did not complete: %v", err)
	}
}

func scheduledJupiterCommand[T any](t *testing.T, ctx context.Context, dsn string, args []string) T {
	t.Helper()
	var result T
	var output bytes.Buffer
	if err := run(ctx, dsn, args, &output, time.Now); err != nil {
		t.Fatalf("paper %s: %v", args[0], err)
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("paper %s invalid receipt: %s", args[0], output.Bytes())
	}
	return result
}

func waitScheduledJupiter(t *testing.T, ctx context.Context, clock func() time.Time, at time.Time) {
	t.Helper()
	for remaining := at.Sub(clock()); remaining > 0; remaining = at.Sub(clock()) {
		timer := time.NewTimer(remaining)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			t.Fatal(ctx.Err())
		}
	}
}

func completeScheduledJupiter(t *testing.T, ctx context.Context, research *jobs.Store, baseURL, jobID, action, notional string) jobs.Result {
	t.Helper()
	workerCtx, stop := context.WithTimeout(ctx, 15*time.Second)
	done := make(chan error, 1)
	go func() {
		done <- research_worker.Run(workerCtx, research_worker.Config{
			BaseURL: baseURL, WorkerID: "jupiter-scheduled-" + strings.ToLower(action), PollInterval: 20 * time.Millisecond,
			HeartbeatInterval: time.Second, RequestTimeout: 3 * time.Second,
		}, func(ctx context.Context, assignment research_worker.Assignment) (json.RawMessage, error) {
			if assignment.JobID != jobID {
				return nil, errors.New("unexpected scheduled fixture assignment")
			}
			return scheduledJupiterAnalysis(ctx, assignment, action, notional)
		})
	}()
	defer func() {
		stop()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		result, err := research.GetResult(workerCtx, jobID)
		if err == nil {
			if result.Attempt != 1 || result.CompletedAt.IsZero() {
				t.Fatalf("fixture did not complete through its first durable lease: %+v", result)
			}
			return result
		}
		if !errors.Is(err, jobs.ErrNotFound) {
			t.Fatalf("read scheduled research result: %v", err)
		}
		select {
		case <-ticker.C:
		case <-workerCtx.Done():
			t.Fatalf("scheduled %s did not complete: %v", action, workerCtx.Err())
		}
	}
}

func scheduledJupiterAnalysis(ctx context.Context, assignment research_worker.Assignment, action, notional string) (json.RawMessage, error) {
	raw, err := research_worker.AnalyzeSynthetic(ctx, assignment)
	if err != nil || action == "HOLD" {
		return raw, err
	}
	var result struct {
		Version    string                       `json:"result_schema_version"`
		WorkerID   string                       `json:"worker_id"`
		LeaseToken string                       `json:"lease_token"`
		Batch      contract.Batch               `json:"batch"`
		Reports    map[string]map[string]string `json:"reports"`
		Metadata   json.RawMessage              `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || len(result.Batch.Intents) != 1 ||
		result.Batch.Intents[0].Symbol != "TSLA" {
		return nil, errors.New("unexpected scheduled Jupiter fixture portfolio")
	}
	intent := &result.Batch.Intents[0]
	intent.Action, intent.Notional = action, notional
	intent.Confidence, intent.Expected = new(0.75), new(2.0)
	intent.Horizon, intent.CostBPS = contract.Horizon{Min: 1, Max: 2}, 25
	report := "Synthetic scheduled Jupiter " + action
	intent.Thesis = []string{report}
	result.Reports[intent.Symbol]["final_trade_decision"] = report
	result.Batch.Evidence[intent.Symbol+":final_trade_decision"] = digestHex([]byte(report))
	return json.Marshal(result)
}

func pinScheduledJupiterQuote(t *testing.T, dir string, result jobs.Result, instrument ledger.Instrument, side, gross string, at time.Time) {
	t.Helper()
	var completion struct {
		Batch contract.Batch `json:"batch"`
	}
	if err := json.Unmarshal(result.Payload, &completion); err != nil || !at.After(result.CompletedAt) {
		t.Fatalf("cannot pin quote after durable decision: %v", err)
	}
	expires := at.Add(time.Minute)
	if completion.Batch.Expires.Before(expires) {
		expires = completion.Batch.Expires
	}
	quote := encode(t, paper_sim.Quote{
		Version: paper_sim.Version, Source: paper_sim.Source, Instrument: instrument, Side: side, QuantityRaw: "100",
		GrossUSD: gross, FeeBPS: new(0), FixedFeeUSD: "0", ObservedAt: at,
		ExpiresAt: expires, Execution: paper_sim.JupiterFixtureExecution,
	})
	name := strings.ToLower(side) + "-jupiter-paper-quote"
	saveDocument(t, dir, name, quote)
	saveDocument(t, dir, result.JobID, encode(t, map[string]any{
		"paper_quote_pin_schema_version": "0.1", "job_id": result.JobID,
		"quote": map[string]string{"ref": name + ".json", "sha256": digestHex(quote)},
	}))
}

func requireScheduledJupiterDispatch(t *testing.T, raw []byte, jobID, status string) paper_workflow.FollowResult {
	t.Helper()
	var result paper_workflow.FollowResult
	if err := json.Unmarshal(raw, &result); err != nil || result.JobID != jobID || result.Status != status {
		t.Fatalf("dispatch %s should be %s: %s", jobID, status, raw)
	}
	return result
}

func requireScheduledJupiterFill(t *testing.T, result paper_workflow.FollowResult, instrument ledger.Instrument, side, limit, gross string) ledger.Order {
	t.Helper()
	if result.Application == nil || result.Application.Execution == nil {
		t.Fatalf("missing ledger fill receipt: %+v", result)
	}
	order := result.Application.Execution.Order
	if order.Status != "filled" || order.Side != side || order.Instrument != instrument ||
		order.GrossUSD != limit || order.QuantityRaw != "100" || order.ResearchJobID != result.JobID ||
		len(order.Fills) != 1 || order.Fills[0].GrossUSD != gross || order.Fills[0].QuantityRaw != "100" || order.Fills[0].FeeUSD != "0" {
		t.Fatalf("invalid %s finalized fill: %+v", side, order)
	}
	return order
}

func requireScheduledJupiterCounts(t *testing.T, ctx context.Context, db *pgx.Conn, attempts, orders, fills, finished int) {
	t.Helper()
	var gotAttempts, gotOrders, gotFills, gotFinished int
	err := db.QueryRow(ctx, `SELECT (SELECT count(*) FROM jupiter_execution_attempts),
		(SELECT count(*) FROM paper_orders), (SELECT count(*) FROM paper_fills),
		(SELECT count(*) FROM research_paper_dispatch WHERE finished_at IS NOT NULL)`).Scan(&gotAttempts, &gotOrders, &gotFills, &gotFinished)
	if err != nil || gotAttempts != attempts || gotOrders != orders || gotFills != fills || gotFinished != finished {
		t.Fatalf("unexpected durable counts: attempts=%d orders=%d fills=%d finished=%d, error=%v",
			gotAttempts, gotOrders, gotFills, gotFinished, err)
	}
}

func scheduledJupiterDatabase(t *testing.T) (context.Context, string, *pgx.Conn) {
	t.Helper()
	dsn := os.Getenv("TRADINGBOT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TRADINGBOT_TEST_DATABASE_URL is required for the scheduled Jupiter rehearsal")
	}
	uri, err := url.Parse(dsn)
	if err != nil || (uri.Scheme != "postgresql" && uri.Scheme != "postgres") ||
		(uri.Hostname() != "127.0.0.1" && uri.Hostname() != "::1") || uri.User == nil || uri.Fragment != "" ||
		strings.Count(uri.Path, "/") != 1 || len(uri.Path) < 2 {
		t.Fatal("scheduled Jupiter rehearsal requires an explicit local PostgreSQL URI")
	}
	port, err := strconv.ParseUint(uri.Port(), 10, 16)
	if err != nil || port == 0 {
		t.Fatal("scheduled Jupiter rehearsal requires one explicit PostgreSQL port")
	}
	params, err := url.ParseQuery(uri.RawQuery)
	if err != nil || len(params) > 1 || len(params["sslmode"]) != len(params) || params.Get("sslmode") != "" && params.Get("sslmode") != "disable" {
		t.Fatal("scheduled Jupiter rehearsal rejects PostgreSQL routing overrides")
	}
	for _, name := range []string{"PGHOST", "PGHOSTADDR", "PGPORT", "PGDATABASE", "PGSERVICE", "PGSERVICEFILE", "PGOPTIONS", "PGTARGETSESSIONATTRS"} {
		if os.Getenv(name) != "" {
			t.Fatalf("clear %s before the isolated PostgreSQL rehearsal", name)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	t.Cleanup(cancel)
	params.Set("sslmode", "disable")
	uri.RawQuery = params.Encode()
	db, err := pgx.Connect(ctx, uri.String())
	if err != nil {
		t.Fatal("connect to scheduled Jupiter test PostgreSQL")
	}
	schema := "jupiter_scheduled_" + strings.ToLower(rand.Text())
	quoted := pgx.Identifier{schema}.Sanitize()
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		defer db.Close(cleanup)
		if _, err := db.Exec(cleanup, "DROP SCHEMA IF EXISTS "+quoted+" CASCADE"); err != nil {
			t.Error("remove isolated scheduled Jupiter schema")
		}
	})
	if _, err := db.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal("create isolated scheduled Jupiter schema")
	}
	if _, err := db.Exec(ctx, "SET search_path TO "+quoted); err != nil {
		t.Fatal("set isolated scheduled Jupiter schema")
	}
	migration, err := os.ReadFile(filepath.Join("..", "..", "..", "venue-quotes", "internal", "jupiter_execution", "001_attempts.sql"))
	if err != nil {
		t.Fatal("read Jupiter test attempts migration")
	}
	if _, err := db.Exec(ctx, string(migration)); err != nil {
		t.Fatal("initialize isolated Jupiter attempts journal")
	}
	params.Set("search_path", schema)
	uri.RawQuery = params.Encode()
	return ctx, uri.String(), db
}

func startScheduledJupiterFixture(t *testing.T, ctx context.Context, dsn string) string {
	t.Helper()
	name := "jupiter-scheduled-fixture"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(t.TempDir(), name)
	build := exec.CommandContext(ctx, "go", "test", "-mod=readonly", "-c", "-o", binary, "./internal/jupiter_rehearsal")
	build.Dir = filepath.Join("..", "..", "..", "venue-quotes")
	build.Env = append(os.Environ(), "GOWORK=off")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build isolated Jupiter test fixture: %v\n%s", err, output)
	}
	child := exec.CommandContext(ctx, binary, "-test.run=^TestScheduledJupiterFixtureServer$")
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		key = strings.ToUpper(key)
		if !strings.HasPrefix(key, "TRADINGBOT_") && !strings.HasPrefix(key, "PG") {
			child.Env = append(child.Env, entry)
		}
	}
	child.Env = append(child.Env, "TRADINGBOT_JUPITER_FIXTURE_SERVER=1", "TRADINGBOT_TEST_DATABASE_URL="+dsn)
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	child.Stderr = io.Discard
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 1)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		defer close(lines)
		reader := bufio.NewScanner(stdout)
		for reader.Scan() {
			if port, ok := strings.CutPrefix(reader.Text(), "JUPITER_FIXTURE_PORT="); ok {
				lines <- port
			}
		}
	}()
	t.Cleanup(func() {
		_ = child.Process.Kill()
		<-drained
		_ = child.Wait()
	})
	startup, stop := context.WithTimeout(ctx, 15*time.Second)
	defer stop()
	select {
	case value := <-lines:
		port, err := strconv.ParseUint(value, 10, 16)
		if err != nil || port == 0 {
			t.Fatal("scheduled Jupiter fixture did not publish one loopback port")
		}
		return "http://127.0.0.1:" + strconv.FormatUint(port, 10)
	case <-startup.Done():
		t.Fatal("scheduled Jupiter fixture did not start")
	}
	return ""
}

func requireScheduledJupiterEvidence(t *testing.T, ctx context.Context, fixtureURL string) {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{}}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fixtureURL+"/v1/paper-fixture/evidence", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal("read offline Jupiter fixture evidence")
	}
	defer response.Body.Close()
	var evidence struct {
		Sends      int    `json:"send_count"`
		Unexpected int    `json:"unexpected_requests"`
		Restarts   int    `json:"restart_count"`
		Initial    string `json:"initial_usdc_raw"`
		PostBuy    string `json:"post_buy_usdc_raw"`
		Final      string `json:"final_usdc_raw"`
		Asset      string `json:"final_asset_raw"`
		InitialSOL string `json:"initial_lamports"`
		FinalSOL   string `json:"final_lamports"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&evidence) != nil ||
		evidence.Sends != 2 || evidence.Unexpected != 0 || evidence.Restarts < 1 ||
		evidence.Initial != "1000000000" || evidence.PostBuy != "800000000" || evidence.Final != "1015000000" ||
		evidence.Asset != "0" || evidence.InitialSOL != "20000000" || evidence.FinalSOL != "19990000" {
		t.Fatalf("unexpected Jupiter paper evidence: HTTP %d, %+v", response.StatusCode, evidence)
	}
}
