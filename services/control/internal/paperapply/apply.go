// Package paperapply authorizes synthetic paper execution from durable research.
package paperapply

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
	"tradingbot/control/internal/paperrisk"
	"tradingbot/control/internal/papersim"
	"tradingbot/control/internal/researchscheduler"
)

const Version = "0.1"

var ErrInvalid = errors.New("invalid paper application")

type ResearchStore interface {
	Get(context.Context, string) (jobs.Job, error)
	GetResult(context.Context, string) (jobs.Result, error)
}

type PaperStore interface {
	papersim.Store
	GetAccount(context.Context, string) (ledger.Account, error)
	GetOrder(context.Context, string, string) (ledger.Order, error)
}

type Request struct {
	Version             string           `json:"paper_apply_schema_version"`
	JobID               string           `json:"job_id"`
	SnapshotContext     SnapshotContext  `json:"snapshot_context"`
	Intent              *papersim.Intent `json:"intent,omitempty"`
	EntryScheduleSHA256 string           `json:"entry_schedule_sha256,omitempty"`
}

type Result struct {
	Version   string           `json:"paper_apply_schema_version"`
	Status    string           `json:"status"`
	JobID     string           `json:"job_id"`
	Execution *papersim.Result `json:"execution,omitempty"`
}

type storedJob struct {
	Version          string                             `json:"job_schema_version"`
	ID               string                             `json:"job_id"`
	Portfolio        json.RawMessage                    `json:"portfolio"`
	RequestedSymbols []string                           `json:"requested_symbols"`
	DataManifest     researchscheduler.ArchiveReference `json:"data_manifest"`
	PolicyVersion    string                             `json:"policy_version"`
	MaxLLMCalls      int                                `json:"max_llm_calls"`
}

type storedResult struct {
	Version  string                       `json:"result_schema_version"`
	Batch    json.RawMessage              `json:"batch"`
	Reports  map[string]map[string]string `json:"reports"`
	Metadata json.RawMessage              `json:"metadata"`
}

// Apply checks immutable job and result receipts, then authorizes one synthetic
// order against the currently locked version at reservation. Replay of an
// existing exact order can still reconcile after its original TTL or version.
func Apply(ctx context.Context, research ResearchStore, paper PaperStore, req Request, rawQuote []byte, now time.Time) (Result, error) {
	if req.Version != Version || !uuidPattern.MatchString(req.JobID) || now.IsZero() {
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
	portfolio, batch, err := loadResearch(job, receipt, req.SnapshotContext, now)
	if err != nil {
		return Result{}, err
	}
	return applyCompleted(ctx, paper, req, rawQuote, portfolio, batch, receipt.CompletedAt, now)
}

func applyCompleted(ctx context.Context, paper PaperStore, req Request, rawQuote []byte, portfolio contract.Portfolio, batch contract.Batch, completedAt, now time.Time) (Result, error) {
	result := Result{Version: Version, JobID: req.JobID}
	if req.Intent == nil && len(rawQuote) == 0 {
		if !allHold(batch.Intents) {
			return Result{}, fmt.Errorf("%w: expected a quote and intent for an actionable batch", ErrInvalid)
		}
		if !batch.Expires.After(now) {
			return Result{}, ledger.ErrExpired
		}
		if _, err := readSnapshot(ctx, paper, req.SnapshotContext, portfolio, now); err != nil {
			return Result{}, err
		}
		result.Status = "hold"
		return result, nil
	}
	var in papersim.Intent
	if req.Intent == nil {
		var err error
		in, err = fromQuote(req.SnapshotContext, batch, completedAt, rawQuote)
		if err != nil {
			return Result{}, err
		}
	} else {
		in = *req.Intent
	}
	if in.AccountID != req.SnapshotContext.AccountID || in.SnapshotVersion != req.SnapshotContext.SnapshotVersion ||
		!in.DecidedAt.Equal(completedAt) || in.ExpiresAt.After(batch.Expires) {
		return Result{}, fmt.Errorf("%w: intent does not match snapshot or result receipt", ErrInvalid)
	}
	preview, err := papersim.Preview(in, rawQuote, now)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := authorize(in, preview, req.SnapshotContext, portfolio, batch); err != nil {
		return Result{}, err
	}
	if err := checkNewOrder(ctx, paper, req.SnapshotContext, portfolio, in, preview, now); err != nil {
		return Result{}, err
	}
	execution, err := papersim.Run(ctx, researchOrderStore{Store: paper, jobID: req.JobID}, in, rawQuote, now)
	if err != nil {
		return Result{}, err
	}
	result.Status, result.Execution = "applied", &execution
	return result, nil
}

// Only the validated research application path attaches job provenance to the
// immutable reservation. The raw paper reserve HTTP contract disallows it.
type researchOrderStore struct {
	papersim.Store
	jobID string
}

func (s researchOrderStore) Reserve(ctx context.Context, request ledger.ReservationRequest) (ledger.Order, bool, error) {
	request.ResearchJobID = s.jobID
	return s.Store.Reserve(ctx, request)
}

func fromQuote(config SnapshotContext, batch contract.Batch, decidedAt time.Time, raw []byte) (papersim.Intent, error) {
	quote, err := papersim.ParseQuote(raw)
	if err != nil {
		return papersim.Intent{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	var symbol string
	for _, rule := range config.Policy.Instruments {
		if rule.Instrument == quote.Instrument {
			symbol = rule.Symbol
			break
		}
	}
	for _, decision := range batch.Intents {
		if decision.Symbol != symbol || decision.Action == "HOLD" {
			continue
		}
		fee, err := quote.FeeUSD()
		if err != nil {
			return papersim.Intent{}, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		hash := sha256.Sum256(raw)
		return papersim.Intent{
			Version: papersim.Version, AccountID: config.AccountID, DecisionID: strings.ToLower(decision.ID), SnapshotVersion: config.SnapshotVersion,
			Instrument: quote.Instrument, Side: quote.Side, QuantityRaw: quote.QuantityRaw, GrossLimitUSD: quote.GrossUSD, MaxFeeUSD: fee,
			DecidedAt: decidedAt, ExpiresAt: batch.Expires, QuoteSHA256: hex.EncodeToString(hash[:]),
		}, nil
	}
	return papersim.Intent{}, fmt.Errorf("%w: quote has no actionable research decision", ErrInvalid)
}

func loadResearch(job jobs.Job, receipt jobs.Result, config SnapshotContext, now time.Time) (contract.Portfolio, contract.Batch, error) {
	var request storedJob
	var result storedResult
	if receipt.JobID != job.ID || receipt.Attempt < 1 || receipt.Attempt > 3 || receipt.CompletedAt.Before(job.CreatedAt) || receipt.CompletedAt.After(now) ||
		contract.Decode(job.Request, nil, &request) != nil || request.Version != "0.1" || request.ID != job.ID || request.PolicyVersion != config.Policy.Version ||
		contract.CheckSymbols(request.RequestedSymbols) != nil || contract.Decode(receipt.Payload, nil, &result) != nil || !knownSource(result, request, receipt.Attempt) {
		return contract.Portfolio{}, contract.Batch{}, fmt.Errorf("%w: unsupported or unbound research receipt", ErrInvalid)
	}
	portfolio, err := contract.ParsePortfolio(request.Portfolio)
	if err != nil || portfolio.AsOf.After(job.CreatedAt) {
		return contract.Portfolio{}, contract.Batch{}, fmt.Errorf("%w: invalid durable portfolio", ErrInvalid)
	}
	if err := config.CheckPortfolio(portfolio); err != nil {
		return contract.Portfolio{}, contract.Batch{}, err
	}
	// The completion path validated its age at receipt. Validate the batch at
	// that immutable instant so an old exact replay keeps its original identity.
	batch, err := contract.ParseBatch(result.Batch, receipt.CompletedAt, receipt.CompletedAt.Sub(portfolio.AsOf))
	if err != nil || contract.CheckAllocation(batch, portfolio, request.RequestedSymbols) != nil || !verifyReports(batch, result.Reports, request.RequestedSymbols) {
		return contract.Portfolio{}, contract.Batch{}, fmt.Errorf("%w: result does not match its durable job or evidence", ErrInvalid)
	}
	return portfolio, batch, nil
}

func knownSource(result storedResult, job storedJob, attempt int) bool {
	var metadata struct {
		Source     string          `json:"source_mode"`
		Alpha      *bool           `json:"alpha_measured"`
		LLMCalls   *int            `json:"llm_calls"`
		PIT        *bool           `json:"intraday_pit_verified"`
		CostUSD    json.RawMessage `json:"usd_cost"`
		Provenance struct {
			JobID        string                             `json:"job_id"`
			Attempt      int                                `json:"attempt"`
			Policy       string                             `json:"policy_version"`
			DataManifest researchscheduler.ArchiveReference `json:"data_manifest"`
		} `json:"provenance"`
	}
	if json.Unmarshal(result.Metadata, &metadata) != nil || metadata.Alpha == nil || *metadata.Alpha || metadata.LLMCalls == nil {
		return false
	}
	switch result.Version {
	case "0.1":
		return metadata.Source == "synthetic_fixture" && *metadata.LLMCalls == 0
	case "0.2":
		p := metadata.Provenance
		return metadata.Source == "tradingagents_offline" && metadata.PIT != nil && !*metadata.PIT && string(metadata.CostUSD) == "null" &&
			*metadata.LLMCalls > 0 && *metadata.LLMCalls <= job.MaxLLMCalls && p.JobID == job.ID && p.Attempt == attempt &&
			p.Policy == job.PolicyVersion && p.DataManifest == job.DataManifest
	default:
		return false
	}
}

func verifyReports(batch contract.Batch, reports map[string]map[string]string, symbols []string) bool {
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

func allHold(intents []contract.Intent) bool {
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

func checkNewOrder(ctx context.Context, paper PaperStore, config SnapshotContext, portfolio contract.Portfolio, in papersim.Intent, preview papersim.FixturePreview, now time.Time) error {
	if _, err := paper.GetOrder(ctx, in.AccountID, preview.OrderID); !errors.Is(err, ledger.ErrNotFound) {
		return err
	}
	if !preview.ExpiresAt.After(now) {
		return ledger.ErrExpired
	}
	account, err := readSnapshot(ctx, paper, config, portfolio, now)
	var activity *ledger.RiskActivity
	if err == nil && config.Policy.RequiresActivity(in.Side) {
		reader, ok := paper.(paperrisk.ActivityReader)
		if !ok {
			return paperrisk.ErrSnapshot
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
	return paperrisk.CheckWithActivity(account, in, config.Policy, config.Marks, now, activity)
}

func authorize(in papersim.Intent, preview papersim.FixturePreview, config SnapshotContext, portfolio contract.Portfolio, batch contract.Batch) error {
	var proposal contract.Intent
	for _, decision := range batch.Intents {
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
		for _, p := range portfolio.Positions {
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
	if feeCap.Cmp(maxCost) > 0 || executionCost(in.Side, quoted, marked, fee).Cmp(maxCost) > 0 {
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
		if executionCost(in.Side, gross, reference, cost).Cmp(allowedCost) > 0 || cost.Cmp(allowedFee) > 0 {
			return fmt.Errorf("%w: partial execution exceeds its cost or fee cap", ErrInvalid)
		}
	}
	return nil
}

func executionCost(side string, gross, reference, fee *big.Rat) *big.Rat {
	adverse := new(big.Rat).Sub(gross, reference)
	if side == "SELL" {
		adverse.Neg(adverse)
	}
	if adverse.Sign() > 0 {
		return adverse.Add(adverse, fee)
	}
	return fee
}
