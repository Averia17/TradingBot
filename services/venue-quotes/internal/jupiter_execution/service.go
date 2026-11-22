package jupiter_execution

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"slices"
	"time"

	solana "github.com/solana-foundation/solana-go/v2"

	"tradingbot/venue-quotes/internal/jupiter"
	"tradingbot/venue-quotes/internal/quotes"
	"tradingbot/venue-quotes/internal/solana_balance"
)

type Receipt struct {
	SchemaVersion        string                            `json:"schema_version"`
	Network              string                            `json:"network"`
	Owner                string                            `json:"owner"`
	IntentID             string                            `json:"intent_id"`
	ReservationID        string                            `json:"reservation_id"`
	ProviderRequestID    string                            `json:"provider_request_id"`
	Signature            string                            `json:"signature"`
	LastValidBlockHeight string                            `json:"last_valid_block_height"`
	ProviderStatus       string                            `json:"provider_status"`
	ProviderCode         *int                              `json:"provider_code"`
	ProviderSlot         *uint64                           `json:"provider_slot"`
	ClaimedAt            time.Time                         `json:"claimed_at"`
	ProviderObservedAt   *time.Time                        `json:"provider_observed_at"`
	ChainStatus          *solana_balance.TransactionStatus `json:"chain_status"`
}

type record struct {
	Receipt
	approvalHash string
}

type journal interface {
	lookup(context.Context, string, string) (record, error)
	claim(context.Context, record) (record, bool, error)
	observe(context.Context, record, jupiter.ExecuteObservation, time.Time) (record, error)
}

type executor interface {
	Execute(context.Context, jupiter.SignedTransaction, string, string) jupiter.ExecuteObservation
}

type chain interface {
	CheckSubmission(context.Context, string, uint64) error
	TransactionStatus(context.Context, string) (solana_balance.TransactionStatus, error)
	Invalidate()
}

type Service struct {
	owner      string
	policyHash string
	key        ed25519.PublicKey
	journal    journal
	provider   executor
	chain      chain
}

func New(store *Postgres, provider *jupiter.Client, rpc *solana_balance.Client, owner, policyHash string, key ed25519.PublicKey) (*Service, error) {
	ownerKey, err := solana.PublicKeyFromBase58(owner)
	if store == nil || provider == nil || rpc == nil || err != nil || !hashPattern.MatchString(policyHash) ||
		!validApprovalKey(key) || bytes.Equal(key, ownerKey[:]) {
		return nil, ErrInvalid
	}
	return &Service{owner: owner, policyHash: policyHash, key: slices.Clone(key), journal: store, provider: provider, chain: rpc}, nil
}

func (service *Service) Submit(ctx context.Context, request Request) (Receipt, error) {
	approval, approvalHash, err := validateApproval(request, service.key, service.owner, service.policyHash)
	if err != nil {
		return Receipt{}, err
	}
	transaction, err := jupiter.ValidateSignedSwap(request.SignedTransaction, approval.Owner)
	if err != nil {
		return Receipt{}, ErrInvalid
	}
	if transaction.SHA256 != approval.SignedTransactionSHA256 {
		return Receipt{}, ErrUnauthorized
	}
	prior, err := service.journal.lookup(ctx, approval.Owner, approval.IntentID)
	if err == nil {
		return prior.match(approvalHash)
	}
	if !errors.Is(err, ErrNotFound) {
		return Receipt{}, err
	}
	if !time.Now().Before(approval.ExpiresAt) {
		return Receipt{}, ErrExpired
	}
	height, _ := quotes.PositiveAmount(approval.LastValidBlockHeight)
	if err := service.chain.CheckSubmission(ctx, transaction.RecentBlockhash, height); err != nil {
		if errors.Is(err, solana_balance.ErrExpired) {
			return Receipt{}, ErrExpired
		}
		return Receipt{}, ErrUnavailable
	}
	now := time.Now().UTC()
	if !now.Before(approval.ExpiresAt) {
		return Receipt{}, ErrExpired
	}
	attempt, claimed, err := service.journal.claim(ctx, record{
		Receipt: Receipt{
			SchemaVersion: "1", Network: quotes.SolanaMainnet, Owner: approval.Owner,
			IntentID: approval.IntentID, ReservationID: approval.ReservationID,
			ProviderRequestID: approval.ProviderRequestID, Signature: transaction.Signature,
			LastValidBlockHeight: approval.LastValidBlockHeight, ProviderStatus: "unknown", ClaimedAt: now,
		},
		approvalHash: approvalHash,
	})
	if err != nil {
		return Receipt{}, err
	}
	if !claimed {
		return attempt.match(approvalHash)
	}
	// A crash, cancellation or failed ledger write after the claim leaves a
	// durable unknown attempt. A replay never submits it again.
	if ctx.Err() != nil || !time.Now().Before(approval.ExpiresAt) {
		return attempt.Receipt, nil
	}
	service.chain.Invalidate()
	defer service.chain.Invalidate()
	sendCtx, cancelSend := context.WithDeadline(ctx, approval.ExpiresAt)
	result := service.provider.Execute(sendCtx, transaction, approval.ProviderRequestID, approval.LastValidBlockHeight)
	cancelSend()
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	saved, err := service.journal.observe(writeCtx, attempt, result, time.Now().UTC())
	if err != nil {
		return attempt.Receipt, nil
	}
	return saved.Receipt, nil
}

func (service *Service) Receipt(ctx context.Context, intentID string) (Receipt, error) {
	if !validID(intentID) {
		return Receipt{}, ErrInvalid
	}
	attempt, err := service.journal.lookup(ctx, service.owner, intentID)
	if err != nil {
		return Receipt{}, err
	}
	status, err := service.chain.TransactionStatus(ctx, attempt.Signature)
	if err == nil {
		attempt.ChainStatus = &status
		if status.State == "finalized_success" || status.State == "finalized_failure" {
			service.chain.Invalidate()
		}
	}
	return attempt.Receipt, nil
}

func (attempt record) match(approvalHash string) (Receipt, error) {
	if attempt.approvalHash != approvalHash {
		return Receipt{}, ErrConflict
	}
	return attempt.Receipt, nil
}
