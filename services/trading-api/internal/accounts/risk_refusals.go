package accounts

import (
	"context"
	"fmt"
	"regexp"
	"time"
)

type RiskRefusalReason string

const (
	RiskInstrumentNotAllowed    RiskRefusalReason = "instrument_not_allowed"
	RiskOrderNotional           RiskRefusalReason = "order_notional"
	RiskMaximumFee              RiskRefusalReason = "maximum_fee"
	RiskMaximumExecutionCost    RiskRefusalReason = "maximum_execution_cost"
	RiskHeldQuantity            RiskRefusalReason = "held_quantity"
	RiskUnresolvedOrder         RiskRefusalReason = "unresolved_order"
	RiskDailyTurnover           RiskRefusalReason = "daily_turnover"
	RiskMinimumFreeCash         RiskRefusalReason = "minimum_free_cash"
	RiskInstrumentConcentration RiskRefusalReason = "instrument_concentration"
	RiskGroupConcentration      RiskRefusalReason = "group_concentration"
	RiskGrossExposure           RiskRefusalReason = "gross_exposure"
	MaxRiskRefusals                               = 100
)

var refusalDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

type RiskRefusal struct {
	DecisionID       string            `json:"decision_id"`
	CandidateOrderID string            `json:"candidate_order_id"`
	SnapshotVersion  int64             `json:"snapshot_version"`
	RequestSHA256    string            `json:"request_sha256"`
	QuoteSHA256      string            `json:"quote_sha256"`
	Reason           RiskRefusalReason `json:"reason"`
	FirstObservedAt  time.Time         `json:"first_observed_at"`
}

type RiskRefusalInput struct {
	RiskRefusal
	AccountID     string
	JobID         string
	EvaluatedAt   time.Time
	ReserveBefore time.Time
}

// RecordRiskRefusal appends an observation at the checked account version.
// It neither binds the research application nor modifies economic state.
func (s *Store) RecordRiskRefusal(ctx context.Context, input RiskRefusalInput) error {
	if !uuidPattern.MatchString(input.AccountID) || !uuidPattern.MatchString(input.JobID) ||
		!uuidPattern.MatchString(input.DecisionID) || !uuidPattern.MatchString(input.CandidateOrderID) ||
		input.SnapshotVersion < 1 || !refusalDigest.MatchString(input.RequestSHA256) ||
		!refusalDigest.MatchString(input.QuoteSHA256) || input.Reason == "" ||
		input.EvaluatedAt.IsZero() || !input.ReserveBefore.After(input.EvaluatedAt) {
		return ErrInvalid
	}
	tx, err := s.write(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	version, err := lockAccount(ctx, tx, input.AccountID)
	if err != nil {
		return err
	}
	if version != input.SnapshotVersion {
		return ErrStale
	}
	var observed time.Time
	if err := tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&observed); err != nil {
		return fmt.Errorf("read paper risk observation time: %w", err)
	}
	if !observed.Before(input.ReserveBefore) {
		return ErrExpired
	}
	if observed.Before(input.EvaluatedAt) ||
		(input.Reason == RiskDailyTurnover && !observed.UTC().Truncate(24*time.Hour).Equal(input.EvaluatedAt.UTC().Truncate(24*time.Hour))) {
		return ErrStale
	}
	if _, err := tx.Exec(ctx, `INSERT INTO paper_risk_refusals
		(account_id,job_id,decision_id,candidate_order_id,snapshot_version,request_sha256,quote_sha256,reason,first_observed_at)
		VALUES ($1,$2,$3,$4,$5,decode($6,'hex'),decode($7,'hex'),$8,$9)
		ON CONFLICT (account_id,job_id,candidate_order_id,request_sha256,reason) DO NOTHING`,
		input.AccountID, input.JobID, input.DecisionID, input.CandidateOrderID, input.SnapshotVersion,
		input.RequestSHA256, input.QuoteSHA256, input.Reason, observed); err != nil {
		return fmt.Errorf("record paper risk refusal: %w", err)
	}
	var exact bool
	if err := tx.QueryRow(ctx, `SELECT decision_id=$6 AND snapshot_version=$7 AND quote_sha256=decode($8,'hex')
		FROM paper_risk_refusals WHERE account_id=$1 AND job_id=$2 AND candidate_order_id=$3
			AND request_sha256=decode($4,'hex') AND reason=$5`, input.AccountID, input.JobID, input.CandidateOrderID,
		input.RequestSHA256, input.Reason, input.DecisionID, input.SnapshotVersion, input.QuoteSHA256).Scan(&exact); err != nil {
		return fmt.Errorf("verify paper risk refusal binding: %w", err)
	}
	if !exact {
		return ErrConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit paper risk refusal: %w", err)
	}
	return nil
}

// RiskRefusals reads the latest bounded observations visible at asOf.
func (s ReadSnapshot) RiskRefusals(ctx context.Context, accountID, jobID string, asOf time.Time) ([]RiskRefusal, bool, error) {
	if !uuidPattern.MatchString(accountID) || !uuidPattern.MatchString(jobID) || asOf.IsZero() || asOf.After(s.cutoff) {
		return nil, false, ErrInvalid
	}
	var exists bool
	if err := s.tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM paper_accounts WHERE account_id=$1)", accountID).Scan(&exists); err != nil {
		return nil, false, fmt.Errorf("read paper risk refusal account: %w", err)
	}
	if !exists {
		return nil, false, ErrNotFound
	}
	rows, err := s.tx.Query(ctx, `SELECT decision_id::text, candidate_order_id::text, snapshot_version,
		encode(request_sha256,'hex'), encode(quote_sha256,'hex'), reason, first_observed_at
		FROM paper_risk_refusals WHERE account_id=$1 AND job_id=$2 AND first_observed_at <= $3
		ORDER BY first_observed_at DESC, candidate_order_id, request_sha256, reason LIMIT $4`,
		accountID, jobID, asOf.UTC(), MaxRiskRefusals+1)
	if err != nil {
		return nil, false, fmt.Errorf("query paper risk refusals: %w", err)
	}
	defer rows.Close()
	refusals := make([]RiskRefusal, 0)
	for rows.Next() {
		var row RiskRefusal
		if err := rows.Scan(&row.DecisionID, &row.CandidateOrderID, &row.SnapshotVersion, &row.RequestSHA256,
			&row.QuoteSHA256, &row.Reason, &row.FirstObservedAt); err != nil {
			return nil, false, fmt.Errorf("read paper risk refusal: %w", err)
		}
		row.FirstObservedAt = row.FirstObservedAt.UTC()
		refusals = append(refusals, row)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("read paper risk refusals: %w", err)
	}
	truncated := len(refusals) > MaxRiskRefusals
	if truncated {
		refusals = refusals[:MaxRiskRefusals]
	}
	return refusals, truncated, nil
}
