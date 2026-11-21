package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/paper_workflow"
)

func apply(ctx context.Context, dsn string, args []string, output io.Writer, clock func() time.Time) error {
	flags := flag.NewFlagSet("paper apply", flag.ContinueOnError)
	policyPath := flags.String("policy", "", "path to owner paper risk policy JSON")
	requestPath := flags.String("request", "", "path to the research decision application request JSON")
	quotePath := flags.String("quote", "", "path to the exact synthetic quote bytes for a trade")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || dsn == "" || *policyPath == "" || *requestPath == "" {
		return errors.New("TRADINGBOT_DATABASE_URL, --policy and --request are required")
	}
	var input paper_workflow.Request
	if err := decodeDocument(*requestPath, contract.PaperApplication, &input); err != nil {
		return fmt.Errorf("read paper application request: %w", err)
	}
	if err := checkOwnerPolicy(*policyPath, input.SnapshotContext.Policy); err != nil {
		return err
	}
	var quote []byte
	if *quotePath != "" {
		var err error
		quote, err = readDocument(*quotePath)
		if err != nil {
			return fmt.Errorf("read paper quote: %w", err)
		}
	}
	research, store, err := openPaperStores(ctx, dsn)
	if err != nil {
		return err
	}
	defer research.Close()
	defer store.Close()
	receipt, err := paper_workflow.Apply(ctx, research, store, input, quote, clock().UTC())
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(receipt)
}
