package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"

	"tradingbot/trading-api/internal/accounts"
	"tradingbot/trading-api/internal/contract"
)

type reconciliationReceipt struct {
	Version       string         `json:"paper_reconciliation_receipt_schema_version"`
	Source        string         `json:"source_mode"`
	ObservationID string         `json:"observation_id"`
	SHA256        string         `json:"observation_sha256"`
	Recorded      bool           `json:"recorded"`
	Order         accounts.Order `json:"order"`
}

func reconcile(ctx context.Context, dsn string, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("paper reconcile", flag.ContinueOnError)
	input := flags.String("observation", "", "path to a local synthetic observation")
	expected := flags.String("sha256", "", "SHA-256 of the observation's exact bytes")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *input == "" || *expected == "" {
		return errors.New("provide --observation and --sha256")
	}
	raw, err := readDocument(*input)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != *expected {
		return errors.New("paper reconciliation observation SHA-256 does not match")
	}
	var observation accounts.ReconciliationObservation
	if contract.Decode(raw, contract.PaperReconciliationObservation, &observation) != nil {
		return errors.New("invalid paper reconciliation observation")
	}
	store, err := openAccountService(ctx, dsn)
	if err != nil {
		return err
	}
	defer store.Close()
	var order accounts.Order
	var recorded bool
	if owner, ok := store.(interface {
		ReconcileDocument(context.Context, []byte, [32]byte) (accounts.Order, bool, error)
	}); ok {
		order, recorded, err = owner.ReconcileDocument(ctx, raw, digest)
	} else {
		order, recorded, err = store.Reconcile(ctx, observation, digest)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(reconciliationReceipt{
		Version: accounts.ReconciliationVersion, Source: observation.Source,
		ObservationID: observation.ObservationID, SHA256: *expected, Recorded: recorded, Order: order,
	})
}
