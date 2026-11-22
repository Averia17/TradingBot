package solana_balance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"time"
)

var (
	ErrInvalidSignature = errors.New("invalid Solana transaction signature")
	ErrExpired          = errors.New("Solana transaction blockhash expired or invalid")
)

type TransactionStatus struct {
	SchemaVersion   string    `json:"schema_version"`
	Network         string    `json:"network"`
	Signature       string    `json:"signature"`
	State           string    `json:"state"`
	ContextSlot     uint64    `json:"context_slot"`
	TransactionSlot *uint64   `json:"transaction_slot,omitempty"`
	FeeLamports     *string   `json:"fee_lamports"`
	ObservedAt      time.Time `json:"observed_at"`
}

type rpcSignatureStatus struct {
	Slot               uint64          `json:"slot"`
	ConfirmationStatus string          `json:"confirmationStatus"`
	Error              json.RawMessage `json:"err"`
}

type rpcTransaction struct {
	Slot        uint64 `json:"slot"`
	Version     *uint8 `json:"version"`
	Transaction struct {
		Signatures []string `json:"signatures"`
	} `json:"transaction"`
	Meta struct {
		Error json.RawMessage `json:"err"`
		Fee   *uint64         `json:"fee"`
	} `json:"meta"`
}

// TransactionStatus observes the current signature status. A finalized result
// requires matching evidence from both status and transaction metadata.
func (client *Client) TransactionStatus(ctx context.Context, signature string) (TransactionStatus, error) {
	if !validBase58(signature, 64, 88) {
		return TransactionStatus{}, ErrInvalidSignature
	}
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	if err := client.checkGenesis(ctx); err != nil {
		return TransactionStatus{}, err
	}
	history := struct {
		SearchTransactionHistory bool `json:"searchTransactionHistory"`
	}{SearchTransactionHistory: true}
	contextSlot, statuses, err := rpcValue[[]*rpcSignatureStatus](ctx, client, "getSignatureStatuses", []string{signature}, history)
	if err != nil {
		return TransactionStatus{}, err
	}
	if len(statuses) != 1 {
		return TransactionStatus{}, ErrUnavailable
	}
	observation := TransactionStatus{
		SchemaVersion: "1", Network: "solana-mainnet", Signature: signature, State: "unknown", ContextSlot: contextSlot,
	}
	if status := statuses[0]; status != nil {
		statusError, valid := decodeTransactionError(status.Error)
		if !valid || status.Slot == 0 || status.Slot > contextSlot {
			return TransactionStatus{}, ErrUnavailable
		}
		observation.TransactionSlot = &status.Slot
		switch status.ConfirmationStatus {
		case "processed", "confirmed":
			observation.State = status.ConfirmationStatus
		case "finalized":
			options := struct {
				Commitment                     string `json:"commitment"`
				Encoding                       string `json:"encoding"`
				MaxSupportedTransactionVersion uint8  `json:"maxSupportedTransactionVersion"`
			}{Commitment: commitment, Encoding: "json", MaxSupportedTransactionVersion: 0}
			transaction, err := rpcLookup[rpcTransaction](ctx, client, "getTransaction", signature, options)
			if err != nil {
				return TransactionStatus{}, err
			}
			if err := observation.applyTransaction(transaction, statusError); err != nil {
				return TransactionStatus{}, err
			}
		default:
			return TransactionStatus{}, ErrUnavailable
		}
	}
	observation.ObservedAt = time.Now().UTC()
	return observation, nil
}

func (observation *TransactionStatus) applyTransaction(transaction *rpcTransaction, statusError any) error {
	if transaction == nil {
		// An archival node can know the signature while its metadata is pruned.
		observation.State = "confirmed"
		return nil
	}
	transactionError, valid := decodeTransactionError(transaction.Meta.Error)
	if transaction.Slot != *observation.TransactionSlot || transaction.Version == nil || *transaction.Version != 0 ||
		len(transaction.Transaction.Signatures) == 0 || transaction.Transaction.Signatures[0] != observation.Signature ||
		transaction.Meta.Fee == nil || !valid || !reflect.DeepEqual(statusError, transactionError) {
		return ErrUnavailable
	}
	observation.State = "finalized_success"
	if statusError != nil {
		observation.State = "finalized_failure"
	}
	observation.FeeLamports = new(strconv.FormatUint(*transaction.Meta.Fee, 10))
	return nil
}

func decodeTransactionError(raw json.RawMessage) (any, bool) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	switch errorValue := value.(type) {
	case nil:
		return nil, true
	case string:
		return value, errorValue != ""
	case map[string]any:
		return value, len(errorValue) == 1
	default:
		return nil, false
	}
}

// CheckSubmission verifies that a prepared Metis transaction can still be
// relayed according to the confirmed height and validity of its blockhash.
func (client *Client) CheckSubmission(ctx context.Context, recentBlockhash string, lastValidBlockHeight uint64) error {
	if !ValidAddress(recentBlockhash) || lastValidBlockHeight == 0 {
		return ErrExpired
	}
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	if err := client.checkGenesis(ctx); err != nil {
		return err
	}
	options := rpcOptions{Commitment: "confirmed"}
	height, err := rpcCall[uint64](ctx, client, "getBlockHeight", options)
	if err != nil {
		return err
	}
	if height == 0 {
		return ErrUnavailable
	}
	if height > lastValidBlockHeight {
		return ErrExpired
	}
	_, valid, err := rpcValue[bool](ctx, client, "isBlockhashValid", recentBlockhash, options)
	if err != nil {
		return err
	}
	if !valid {
		return ErrExpired
	}
	return nil
}
