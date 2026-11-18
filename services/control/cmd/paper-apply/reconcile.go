package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/ledger"
)

type reconciliationReceipt struct {
	Version       string       `json:"paper_reconciliation_receipt_schema_version"`
	Source        string       `json:"source_mode"`
	ObservationID string       `json:"observation_id"`
	SHA256        string       `json:"observation_sha256"`
	Recorded      bool         `json:"recorded"`
	Order         ledger.Order `json:"order"`
}

func reconcile(ctx context.Context, dsn string, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("paper-apply reconcile", flag.ContinueOnError)
	input := flags.String("observation", "", "path to a local synthetic observation")
	expected := flags.String("sha256", "", "SHA-256 of the observation's exact bytes")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *input == "" || *expected == "" || dsn == "" {
		return errors.New("provide --observation, --sha256 and TRADINGBOT_DATABASE_URL")
	}
	raw, err := readDocument(*input)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != *expected {
		return errors.New("paper reconciliation observation SHA-256 does not match")
	}
	var observation ledger.ReconciliationObservation
	if contract.Decode(raw, contract.PaperReconciliationObservation, &observation) != nil {
		return errors.New("invalid paper reconciliation observation")
	}
	store, err := ledger.Open(ctx, dsn)
	if err != nil {
		return errors.New("paper storage startup failed; check PostgreSQL configuration")
	}
	defer store.Close()
	order, recorded, err := store.Reconcile(ctx, observation, digest)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(reconciliationReceipt{
		Version: ledger.ReconciliationVersion, Source: observation.Source,
		ObservationID: observation.ObservationID, SHA256: *expected, Recorded: recorded, Order: order,
	})
}
