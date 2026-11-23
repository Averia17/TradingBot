package accounts_http

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"time"

	"tradingbot/trading-api/internal/accounts"
)

const (
	internalPath    = "/internal/v1/paper/"
	maxRequestBody  = 1 << 20
	maxResponseBody = 8 << 20
	requestTimeout  = 5 * time.Second
)

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type application struct {
	JobID           string    `json:"job_id"`
	AccountID       string    `json:"account_id"`
	SnapshotVersion int64     `json:"snapshot_version"`
	RequestSHA256   string    `json:"request_sha256"`
	Quote           []byte    `json:"quote_base64,omitempty"`
	ReserveBefore   time.Time `json:"reserve_before,omitzero"`
}

func fromApplication(v accounts.ApplicationInput) application {
	return application{
		JobID: v.JobID, AccountID: v.AccountID, SnapshotVersion: v.SnapshotVersion,
		RequestSHA256: hex.EncodeToString(v.RequestSHA256[:]), Quote: v.Quote, ReserveBefore: v.ReserveBefore,
	}
}

func (v application) domain() (accounts.ApplicationInput, error) {
	if !digestPattern.MatchString(v.RequestSHA256) {
		return accounts.ApplicationInput{}, accounts.ErrInvalid
	}
	var digest [sha256.Size]byte
	if _, err := hex.Decode(digest[:], []byte(v.RequestSHA256)); err != nil {
		return accounts.ApplicationInput{}, accounts.ErrInvalid
	}
	return accounts.ApplicationInput{
		JobID: v.JobID, AccountID: v.AccountID, SnapshotVersion: v.SnapshotVersion,
		RequestSHA256: digest, Quote: v.Quote, ReserveBefore: v.ReserveBefore,
	}, nil
}

type applicationRecord struct {
	Application application `json:"application"`
	OrderID     string      `json:"order_id,omitempty"`
	Result      []byte      `json:"result_base64,omitempty"`
}

func fromApplicationRecord(v accounts.ApplicationRecord) applicationRecord {
	return applicationRecord{Application: fromApplication(v.ApplicationInput), OrderID: v.OrderID, Result: v.Result}
}

func (v applicationRecord) domain() (accounts.ApplicationRecord, error) {
	input, err := v.Application.domain()
	if err != nil {
		return accounts.ApplicationRecord{}, err
	}
	return accounts.ApplicationRecord{ApplicationInput: input, OrderID: v.OrderID, Result: v.Result}, nil
}

type reserveApplication struct {
	Order       accounts.ReservationRequest `json:"order"`
	Application application                 `json:"application"`
}

type recordApplication struct {
	Application application `json:"application"`
	OrderID     string      `json:"order_id,omitempty"`
	Result      []byte      `json:"result_base64"`
	ExpiresAt   time.Time   `json:"expires_at"`
}

type orderWrite struct {
	Order    accounts.Order `json:"order"`
	Recorded bool           `json:"recorded"`
}

type riskRefusal struct {
	AccountID        string                     `json:"account_id"`
	JobID            string                     `json:"job_id"`
	DecisionID       string                     `json:"decision_id"`
	CandidateOrderID string                     `json:"candidate_order_id"`
	SnapshotVersion  int64                      `json:"snapshot_version"`
	RequestSHA256    string                     `json:"request_sha256"`
	QuoteSHA256      string                     `json:"quote_sha256"`
	Reason           accounts.RiskRefusalReason `json:"reason"`
	EvaluatedAt      time.Time                  `json:"evaluated_at"`
	ReserveBefore    time.Time                  `json:"reserve_before"`
}

func fromRiskRefusal(v accounts.RiskRefusalInput) riskRefusal {
	return riskRefusal{
		AccountID: v.AccountID, JobID: v.JobID, DecisionID: v.DecisionID, CandidateOrderID: v.CandidateOrderID,
		SnapshotVersion: v.SnapshotVersion, RequestSHA256: v.RequestSHA256, QuoteSHA256: v.QuoteSHA256,
		Reason: v.Reason, EvaluatedAt: v.EvaluatedAt, ReserveBefore: v.ReserveBefore,
	}
}

func (v riskRefusal) domain() accounts.RiskRefusalInput {
	return accounts.RiskRefusalInput{
		AccountID: v.AccountID, JobID: v.JobID, EvaluatedAt: v.EvaluatedAt, ReserveBefore: v.ReserveBefore,
		RiskRefusal: accounts.RiskRefusal{
			DecisionID: v.DecisionID, CandidateOrderID: v.CandidateOrderID, SnapshotVersion: v.SnapshotVersion,
			RequestSHA256: v.RequestSHA256, QuoteSHA256: v.QuoteSHA256, Reason: v.Reason,
		},
	}
}

type reconciliation struct {
	Observation []byte `json:"observation_base64"`
	SHA256      string `json:"observation_sha256"`
}
