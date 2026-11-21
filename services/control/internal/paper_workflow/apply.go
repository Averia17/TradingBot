// Package paper_workflow prepares paper snapshots, schedules entry research,
// and applies completed research to the paper ledger.
package paper_workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paper_risk"
	"tradingbot/control/internal/paper_sim"
	"tradingbot/control/internal/research_scheduler"
)

const Version = "0.1"

var ErrInvalid = errors.New("invalid paper application")

type ResearchStore interface {
	Get(context.Context, string) (jobs.Job, error)
	GetResult(context.Context, string) (jobs.Result, error)
}

type PaperStore interface {
	paper_sim.Store
	GetAccount(context.Context, string) (ledger.Account, error)
	GetOrder(context.Context, string, string) (ledger.Order, error)
	RecordRiskRefusal(context.Context, ledger.RiskRefusalInput) error
}

type Request struct {
	Version              string            `json:"paper_apply_schema_version"`
	JobID                string            `json:"job_id"`
	SnapshotContext      SnapshotContext   `json:"snapshot_context"`
	Intent               *paper_sim.Intent `json:"intent,omitempty"`
	EntryScheduleSHA256  string            `json:"entry_schedule_sha256,omitempty"`
	ReviewScheduleSHA256 string            `json:"review_schedule_sha256,omitempty"`
	ReviewOrigin         *ReviewOrigin     `json:"review_origin,omitempty"`
	Identity             *IdentityBinding  `json:"identity_binding,omitempty"`
}

type Result struct {
	Version   string            `json:"paper_apply_schema_version"`
	Status    string            `json:"status"`
	JobID     string            `json:"job_id"`
	Execution *paper_sim.Result `json:"execution,omitempty"`
}

type storedJob struct {
	Version          string                              `json:"job_schema_version"`
	ID               string                              `json:"job_id"`
	Portfolio        json.RawMessage                     `json:"portfolio"`
	RequestedSymbols []string                            `json:"requested_symbols"`
	DataManifest     research_scheduler.ArchiveReference `json:"data_manifest"`
	PolicyVersion    string                              `json:"policy_version"`
	MaxLLMCalls      int                                 `json:"max_llm_calls"`
	IdentityRegistry *ArchiveReference                   `json:"identity_registry,omitempty"`
	SecurityBindings []ResearchSecurityBinding           `json:"security_bindings,omitempty"`
}

type storedResult struct {
	Version  string                       `json:"result_schema_version"`
	Batch    json.RawMessage              `json:"batch"`
	Reports  map[string]map[string]string `json:"reports"`
	Metadata json.RawMessage              `json:"metadata"`
}

// applicationPlan holds the validated research and snapshot for a paper action.
// It can be inspected for quote selection or applied to the versioned ledger.
type applicationPlan struct {
	request     Request
	portfolio   contract.Portfolio
	batch       contract.Batch
	completedAt time.Time
}

// Apply checks immutable job and result receipts, then authorizes one synthetic
// order against the currently locked version at reservation. Replay of an
// existing exact order can still reconcile after its original TTL or version.
func Apply(ctx context.Context, research ResearchStore, paper PaperStore, req Request, rawQuote []byte, now time.Time) (Result, error) {
	if req.Version != Version || req.Identity != nil || req.ReviewScheduleSHA256 != "" || req.ReviewOrigin != nil ||
		!uuidPattern.MatchString(req.JobID) || now.IsZero() {
		return Result{}, ErrInvalid
	}
	job, err := research.Get(ctx, req.JobID)
	if err != nil {
		return Result{}, fmt.Errorf("read durable research job: %w", err)
	}
	if job.ID != req.JobID || job.Status != "completed" || job.CreatedAt.IsZero() {
		return Result{}, fmt.Errorf("%w: research job has no completion", ErrInvalid)
	}
	receipt, err := research.GetResult(ctx, req.JobID)
	if err != nil {
		return Result{}, fmt.Errorf("read durable research result: %w", err)
	}
	application, err := validateCompletedResearch(job, receipt, req, now)
	if err != nil {
		return Result{}, err
	}
	return application.execute(ctx, paper, rawQuote, now)
}

func (a applicationPlan) execute(ctx context.Context, paper PaperStore, rawQuote []byte, now time.Time) (Result, error) {
	req := a.request
	result := Result{Version: req.Version, JobID: req.JobID}
	if req.Intent == nil && len(rawQuote) == 0 {
		if !a.isHold() {
			return Result{}, fmt.Errorf("%w: expected a quote and intent for an actionable batch", ErrInvalid)
		}
		if !a.batch.Expires.After(now) {
			return Result{}, ledger.ErrExpired
		}
		if _, err := readSnapshot(ctx, paper, req.SnapshotContext, a.portfolio, now); err != nil {
			return Result{}, err
		}
		result.Status = "hold"
		return result, nil
	}
	order, err := a.authorizedOrder(rawQuote, now)
	if err != nil {
		return Result{}, err
	}
	if err := checkNewOrder(ctx, paper, req.SnapshotContext, a.portfolio, order.intent, order.preview, now); err != nil {
		if reason, ok := paper_risk.RefusalReason(err); ok {
			if recordErr := recordRiskRefusal(ctx, paper, req, order.intent, order.preview, reason, now); recordErr != nil {
				return Result{}, errors.Join(err, fmt.Errorf("persist paper risk refusal: %w", recordErr))
			}
		}
		return Result{}, err
	}
	execution, err := order.fixture.Run(ctx, researchOrderStore{Store: paper, jobID: req.JobID})
	if err != nil {
		return Result{}, err
	}
	result.Status, result.Execution = "applied", &execution
	return result, nil
}

type preparedPaperOrder struct {
	intent  paper_sim.Intent
	preview paper_sim.FixturePreview
	fixture *paper_sim.PreparedFixture
}

func (a applicationPlan) prepareOrder(raw []byte, now time.Time) (preparedPaperOrder, error) {
	var intent paper_sim.Intent
	var fixture *paper_sim.PreparedFixture
	var err error
	if a.request.Intent != nil {
		intent = *a.request.Intent
		config := a.request.SnapshotContext
		if intent.AccountID != config.AccountID || intent.SnapshotVersion != config.SnapshotVersion ||
			!intent.DecidedAt.Equal(a.completedAt) || intent.ExpiresAt.After(a.batch.Expires) {
			return preparedPaperOrder{}, fmt.Errorf("%w: intent does not match snapshot or result receipt", ErrInvalid)
		}
		fixture, err = paper_sim.Prepare(intent, raw, now)
	} else {
		quote, decodeErr := paper_sim.DecodeFixtureQuote(raw)
		if decodeErr != nil {
			return preparedPaperOrder{}, fmt.Errorf("%w: %v", ErrInvalid, decodeErr)
		}
		intent, err = a.intentFromQuote(quote.Fields(), quote.SHA256())
		if err != nil {
			return preparedPaperOrder{}, err
		}
		fixture, err = quote.Prepare(intent, now)
	}
	if err != nil {
		return preparedPaperOrder{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return preparedPaperOrder{intent: intent, preview: fixture.Preview(), fixture: fixture}, nil
}

func (a applicationPlan) authorizedOrder(raw []byte, now time.Time) (preparedPaperOrder, error) {
	order, err := a.prepareOrder(raw, now)
	if err != nil {
		return preparedPaperOrder{}, err
	}
	if err := a.authorize(order.intent, order.preview); err != nil {
		return preparedPaperOrder{}, err
	}
	return order, nil
}

func recordRiskRefusal(ctx context.Context, paper PaperStore, req Request, in paper_sim.Intent, preview paper_sim.FixturePreview, reason ledger.RiskRefusalReason, now time.Time) error {
	deadline := req.deadline(preview.ExpiresAt)
	if !deadline.After(now) {
		return nil
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encode paper application request: %w", err)
	}
	err = paper.RecordRiskRefusal(ctx, ledger.RiskRefusalInput{
		AccountID: in.AccountID, JobID: req.JobID, EvaluatedAt: now, ReserveBefore: deadline,
		RiskRefusal: ledger.RiskRefusal{
			DecisionID: in.DecisionID, CandidateOrderID: preview.OrderID, SnapshotVersion: in.SnapshotVersion,
			RequestSHA256: fmt.Sprintf("%x", sha256.Sum256(raw)), QuoteSHA256: in.QuoteSHA256, Reason: reason,
		},
	})
	if errors.Is(err, ledger.ErrStale) || errors.Is(err, ledger.ErrExpired) {
		// A racing mutation or expiry invalidates the observation, not the
		// caller's original refusal. Leave the existing retry policy intact.
		return nil
	}
	return err
}

// Only the validated research application path attaches job provenance to the
// immutable reservation. The raw paper reserve HTTP contract disallows it.
type researchOrderStore struct {
	paper_sim.Store
	jobID string
}

func (s researchOrderStore) Reserve(ctx context.Context, request ledger.ReservationRequest) (ledger.Order, bool, error) {
	request.ResearchJobID = s.jobID
	return s.Store.Reserve(ctx, request)
}

func (a applicationPlan) intentFromQuote(quote paper_sim.Quote, digest string) (paper_sim.Intent, error) {
	req := a.request
	config := req.SnapshotContext
	var symbol string
	for _, rule := range config.Policy.Instruments {
		if rule.Instrument == quote.Instrument {
			symbol = rule.Symbol
			break
		}
	}
	for _, decision := range a.batch.Intents {
		if decision.Symbol != symbol || decision.Action == "HOLD" {
			continue
		}
		fee, err := quote.FeeUSD()
		if err != nil {
			return paper_sim.Intent{}, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		intent := paper_sim.Intent{
			Version: paper_sim.Version, AccountID: config.AccountID, DecisionID: strings.ToLower(decision.ID), SnapshotVersion: config.SnapshotVersion,
			Instrument: quote.Instrument, Side: quote.Side, QuantityRaw: quote.QuantityRaw, GrossLimitUSD: quote.GrossUSD, MaxFeeUSD: fee,
			DecidedAt: a.completedAt, ExpiresAt: a.batch.Expires, QuoteSHA256: digest,
		}
		if req.Identity == nil {
			if quote.SecurityID != "" || quote.IdentityRegistrySHA256 != "" {
				return paper_sim.Intent{}, ErrInvalid
			}
			return intent, nil
		}
		for _, security := range req.Identity.Securities {
			if quote.Version == paper_sim.IdentityQuoteVersion && security.Symbol == symbol && security.Instrument == quote.Instrument &&
				security.SecurityID == quote.SecurityID && req.Identity.Registry.SHA256 == quote.IdentityRegistrySHA256 {
				intent.Version = paper_sim.IdentityIntentVersion
				intent.SecurityID, intent.IdentityRegistrySHA256 = quote.SecurityID, quote.IdentityRegistrySHA256
				intent.ExpiresAt = req.deadline(intent.ExpiresAt)
				return intent, nil
			}
		}
		return paper_sim.Intent{}, fmt.Errorf("%w: quote differs from the owner security binding", ErrInvalid)
	}
	return paper_sim.Intent{}, fmt.Errorf("%w: quote has no actionable research decision", ErrInvalid)
}

func validateCompletedResearch(job jobs.Job, receipt jobs.Result, req Request, now time.Time) (applicationPlan, error) {
	config := req.SnapshotContext
	var request storedJob
	var result storedResult
	if receipt.JobID != job.ID || receipt.Attempt < 1 || receipt.Attempt > 3 || receipt.CompletedAt.Before(job.CreatedAt) || receipt.CompletedAt.After(now) ||
		contract.Decode(job.Request, nil, &request) != nil || request.ID != job.ID || request.PolicyVersion != config.Policy.Version ||
		contract.CheckSymbols(request.RequestedSymbols) != nil || contract.Decode(receipt.Payload, nil, &result) != nil || !validResultProvenance(result, request, receipt.Attempt) {
		return applicationPlan{}, fmt.Errorf("%w: unsupported or unbound research receipt", ErrInvalid)
	}
	portfolio, err := contract.ParsePortfolio(request.Portfolio)
	if err != nil || portfolio.AsOf.After(job.CreatedAt) {
		return applicationPlan{}, fmt.Errorf("%w: invalid durable portfolio", ErrInvalid)
	}
	if err := config.CheckPortfolio(portfolio); err != nil {
		return applicationPlan{}, err
	}
	if !req.matchesIdentity(request, result, portfolio) {
		return applicationPlan{}, fmt.Errorf("%w: research receipt differs from the owner identity binding", ErrInvalid)
	}
	// The completion path validated its age at receipt. Validate the batch at
	// that immutable instant so an old exact replay keeps its original identity.
	batch, err := contract.ParseBatch(result.Batch, receipt.CompletedAt, receipt.CompletedAt.Sub(portfolio.AsOf))
	if err != nil || contract.CheckAllocation(batch, portfolio, request.RequestedSymbols) != nil || !reportsMatchEvidence(batch, result.Reports, request.RequestedSymbols) {
		return applicationPlan{}, fmt.Errorf("%w: result does not match its durable job or evidence", ErrInvalid)
	}
	return applicationPlan{request: req, portfolio: portfolio, batch: batch, completedAt: receipt.CompletedAt}, nil
}

func validResultProvenance(result storedResult, job storedJob, attempt int) bool {
	var metadata struct {
		Source     string          `json:"source_mode"`
		Alpha      *bool           `json:"alpha_measured"`
		LLMCalls   *int            `json:"llm_calls"`
		PIT        *bool           `json:"intraday_pit_verified"`
		CostUSD    json.RawMessage `json:"usd_cost"`
		Provenance struct {
			JobID        string                              `json:"job_id"`
			Attempt      int                                 `json:"attempt"`
			Policy       string                              `json:"policy_version"`
			DataManifest research_scheduler.ArchiveReference `json:"data_manifest"`
		} `json:"provenance"`
	}
	if json.Unmarshal(result.Metadata, &metadata) != nil || metadata.Alpha == nil || *metadata.Alpha || metadata.LLMCalls == nil {
		return false
	}
	switch result.Version {
	case "0.1":
		return metadata.Source == "synthetic_fixture" && *metadata.LLMCalls == 0
	case "0.2", "0.3":
		p := metadata.Provenance
		return metadata.Source == "tradingagents_offline" && metadata.PIT != nil && !*metadata.PIT && string(metadata.CostUSD) == "null" &&
			*metadata.LLMCalls > 0 && *metadata.LLMCalls <= job.MaxLLMCalls && p.JobID == job.ID && p.Attempt == attempt &&
			p.Policy == job.PolicyVersion && p.DataManifest == job.DataManifest
	default:
		return false
	}
}

func reportsMatchEvidence(batch contract.Batch, reports map[string]map[string]string, symbols []string) bool {
	if len(reports) != len(symbols) {
		return false
	}
	count := 0
	for _, symbol := range symbols {
		if strings.TrimSpace(reports[symbol]["final_trade_decision"]) == "" {
			return false
		}
		for name, report := range reports[symbol] {
			digest := sha256.Sum256([]byte(report))
			if strings.TrimSpace(report) == "" || batch.Evidence[symbol+":"+name] != hex.EncodeToString(digest[:]) {
				return false
			}
			count++
		}
	}
	return count == len(batch.Evidence)
}

func (a applicationPlan) isHold() bool {
	intents := a.batch.Intents
	if len(intents) == 0 {
		return false
	}
	for _, in := range intents {
		if in.Action != "HOLD" {
			return false
		}
	}
	return true
}

func (a applicationPlan) deadline() time.Time { return a.request.deadline(a.batch.Expires) }

func readSnapshot(ctx context.Context, paper PaperStore, config SnapshotContext, portfolio contract.Portfolio, now time.Time) (ledger.Account, error) {
	if err := config.CheckMarks(now); err != nil {
		return ledger.Account{}, err
	}
	account, err := paper.GetAccount(ctx, config.AccountID)
	if err != nil {
		return ledger.Account{}, fmt.Errorf("read paper account: %w", err)
	}
	current, err := Snapshot(account, config, portfolio.AsOf)
	if err != nil {
		return ledger.Account{}, err
	}
	if current.ID != portfolio.ID {
		return ledger.Account{}, ledger.ErrStale
	}
	return account, nil
}

func checkNewOrder(ctx context.Context, paper PaperStore, config SnapshotContext, portfolio contract.Portfolio, in paper_sim.Intent, preview paper_sim.FixturePreview, now time.Time) error {
	if _, err := paper.GetOrder(ctx, in.AccountID, preview.OrderID); !errors.Is(err, ledger.ErrNotFound) {
		return err
	}
	if !preview.ExpiresAt.After(now) {
		return ledger.ErrExpired
	}
	account, err := readSnapshot(ctx, paper, config, portfolio, now)
	var activity *ledger.RiskActivity
	if err == nil && config.Policy.RequiresActivity(in.Side) {
		reader, ok := paper.(paper_risk.ActivityReader)
		if !ok {
			return paper_risk.ErrSnapshot
		}
		var current ledger.RiskActivity
		current, err = reader.RiskActivity(ctx, account.ID, account.Version)
		if err == nil {
			activity = &current
		}
	}
	if errors.Is(err, ledger.ErrStale) {
		// The same reservation can commit between reading its ID and the
		// portfolio. Let the ledger check the exact request hash on replay.
		if _, orderErr := paper.GetOrder(ctx, in.AccountID, preview.OrderID); !errors.Is(orderErr, ledger.ErrNotFound) {
			return orderErr
		}
	}
	if err != nil {
		return err
	}
	return paper_risk.CheckOrder(account, in, config.Policy, config.Marks, now, activity, preview.Fill)
}

func (a applicationPlan) authorize(in paper_sim.Intent, preview paper_sim.FixturePreview) error {
	config := a.request.SnapshotContext
	var proposal contract.Intent
	for _, decision := range a.batch.Intents {
		if strings.EqualFold(decision.ID, in.DecisionID) {
			proposal = decision
			break
		}
	}
	switch proposal.Action {
	case "BUY", "ADD":
		if in.Side != "BUY" {
			return fmt.Errorf("%w: wrong action side", ErrInvalid)
		}
	case "SELL", "REDUCE":
		if in.Side != "SELL" {
			return fmt.Errorf("%w: wrong action side", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: no executable decision for intent", ErrInvalid)
	}
	var price *big.Rat
	for _, mark := range config.Marks {
		if mark.Instrument != in.Instrument {
			continue
		}
		for _, rule := range config.Policy.Instruments {
			if rule.Instrument == mark.Instrument && rule.Symbol == proposal.Symbol {
				price, _ = contract.Amount(mark.PriceUSD)
				break
			}
		}
		break
	}
	if price == nil {
		return fmt.Errorf("%w: decision symbol does not match a marked instrument", ErrInvalid)
	}
	if proposal.Action != "BUY" {
		matching := false
		for _, p := range a.portfolio.Positions {
			if p.Symbol == proposal.Symbol && p.Instrument == in.Instrument.ID {
				matching = true
				break
			}
		}
		if !matching {
			return fmt.Errorf("%w: decision instrument differs from held position", ErrInvalid)
		}
	}
	quantity, err := rawAmount(in.QuantityRaw, in.Instrument.Decimals)
	if err != nil {
		return err
	}
	marked := new(big.Rat).Mul(quantity, price)
	approved, _ := contract.Amount(proposal.Notional)
	// The fixture and the policy already validated wire grammar for these amounts.
	limit, _ := new(big.Rat).SetString(in.GrossLimitUSD)
	feeCap, _ := new(big.Rat).SetString(in.MaxFeeUSD)
	quoted, _ := new(big.Rat).SetString(preview.GrossUSD)
	fee, _ := new(big.Rat).SetString(preview.FeeUSD)
	maxOrder, _ := new(big.Rat).SetString(config.Policy.MaxOrderUSD)
	if quoted.Cmp(maxOrder) > 0 {
		return fmt.Errorf("%w: quoted gross exceeds paper order policy", ErrInvalid)
	}
	if marked.Cmp(approved) > 0 || (in.Side == "BUY" && limit.Cmp(approved) > 0) ||
		(proposal.Action == "SELL" && marked.Cmp(approved) != 0) {
		return fmt.Errorf("%w: intent size differs from decision", ErrInvalid)
	}
	maxCost := new(big.Rat).Mul(marked, big.NewRat(int64(proposal.CostBPS), 10000))
	if feeCap.Cmp(maxCost) > 0 || paper_risk.AdverseExecutionCost(in.Side, quoted, marked, fee).Cmp(maxCost) > 0 {
		return fmt.Errorf("%w: paper quote exceeds decision execution cost", ErrInvalid)
	}
	if fill := preview.Fill; fill != nil && fill.QuantityRaw != in.QuantityRaw {
		units, err := rawAmount(fill.QuantityRaw, in.Instrument.Decimals)
		if err != nil {
			return err
		}
		reference := new(big.Rat).Mul(units, price)
		gross, _ := new(big.Rat).SetString(fill.GrossUSD)
		cost, _ := new(big.Rat).SetString(fill.FeeUSD)
		allowedCost := new(big.Rat).Mul(reference, big.NewRat(int64(proposal.CostBPS), 10000))
		feeBasis := reference
		if gross.Cmp(feeBasis) < 0 {
			feeBasis = gross
		}
		allowedFee := new(big.Rat).Mul(feeBasis, big.NewRat(int64(config.Policy.MaxFeeBPS), 10000))
		if paper_risk.AdverseExecutionCost(in.Side, gross, reference, cost).Cmp(allowedCost) > 0 || cost.Cmp(allowedFee) > 0 {
			return fmt.Errorf("%w: partial execution exceeds its cost or fee cap", ErrInvalid)
		}
	}
	return nil
}
