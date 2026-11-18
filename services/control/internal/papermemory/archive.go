package papermemory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paperrisk"
)

const (
	MaxArchiveBytes             = 16 * 1024 * 1024
	MaxArchiveOutcomesPerSymbol = 4
	outcomeKind                 = "paper_outcome"
	outcomeProvider             = "paper-ledger"
)

type archiveDocument struct {
	Version    string            `json:"schema_version"`
	SourceMode string            `json:"source_mode"`
	Records    []json.RawMessage `json:"records"`
}

type paperArchiveRecord struct {
	Kind        string          `json:"kind"`
	Provider    string          `json:"provider"`
	RecordID    string          `json:"record_id"`
	Symbol      string          `json:"symbol"`
	Revision    int             `json:"revision"`
	EventTime   time.Time       `json:"event_time"`
	AvailableAt time.Time       `json:"available_at"`
	IngestedAt  time.Time       `json:"ingested_at"`
	Payload     archivedOutcome `json:"payload"`
}

type archivedOutcome struct {
	SchemaVersion    string            `json:"schema_version"`
	SourceMode       string            `json:"source_mode"`
	AccountingMethod string            `json:"accounting_method"`
	AccountID        string            `json:"account_id"`
	AccountVersion   int64             `json:"account_version"`
	FillEventID      string            `json:"fill_event_id"`
	DecisionID       string            `json:"decision_id"`
	PolicyVersion    string            `json:"policy_version"`
	RiskGroup        string            `json:"risk_group"`
	Instrument       ledger.Instrument `json:"instrument"`
	QuantityRaw      string            `json:"quantity_raw"`
	GrossUSD         string            `json:"gross_usd"`
	FeeUSD           string            `json:"fee_usd"`
	CostBasisUSD     string            `json:"cost_basis_usd"`
	NetPnLUSD        string            `json:"net_pnl_usd"`
}

// ExportArchive appends recent realized outcomes from the ledger prefix to a
// hash-verified local archive. An existing archive cannot supply ledger records.
func ExportArchive(ctx context.Context, reader EventReader, accountID string, asOf time.Time, policy paperrisk.Policy, raw []byte, expectedDigest string) ([]byte, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	archive, err := readMarketArchive(raw, expectedDigest)
	if err != nil {
		return nil, err
	}
	view, err := Load(ctx, reader, accountID, asOf)
	if err != nil {
		return nil, err
	}
	rules := make(map[ledger.Instrument]paperrisk.InstrumentRule, len(policy.Instruments))
	for _, rule := range policy.Instruments {
		rules[rule.Instrument] = rule
	}
	perSymbol := make(map[string]int, len(rules))
	for i := len(view.Realized) - 1; i >= 0; i-- {
		outcome := view.Realized[i]
		rule, eligible := rules[outcome.Instrument]
		if !eligible || perSymbol[rule.Symbol] >= MaxArchiveOutcomesPerSymbol {
			continue
		}
		// The FIFO result is reproducible as soon as its complete ledger
		// prefix is known. The ledger receipt is its local ingestion time.
		record := paperArchiveRecord{
			Kind: outcomeKind, Provider: outcomeProvider, RecordID: view.AccountID + "-" + outcome.FillEventID,
			Symbol: rule.Symbol, Revision: 1, EventTime: outcome.ObservedAt,
			AvailableAt: outcome.AvailableAt, IngestedAt: outcome.AvailableAt,
			Payload: archivedOutcome{
				SchemaVersion: "0.1", SourceMode: sourceMode, AccountingMethod: accountingMethod,
				AccountID: view.AccountID, AccountVersion: outcome.accountVersion,
				FillEventID: outcome.FillEventID, DecisionID: outcome.DecisionID,
				PolicyVersion: policy.Version, RiskGroup: rule.Group, Instrument: outcome.Instrument,
				QuantityRaw: outcome.QuantityRaw, GrossUSD: outcome.GrossUSD, FeeUSD: outcome.FeeUSD,
				CostBasisUSD: outcome.CostBasisUSD, NetPnLUSD: outcome.NetPnLUSD,
			},
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			return nil, err
		}
		archive.Records = append(archive.Records, encoded)
		perSymbol[rule.Symbol]++
	}
	if len(perSymbol) == 0 {
		return raw, nil
	}
	archive.Version = "0.2"
	output, err := json.Marshal(archive)
	if err != nil {
		return nil, err
	}
	if len(output) > MaxArchiveBytes {
		return nil, errors.New("research archive with paper outcomes exceeds the byte limit")
	}
	return output, nil
}

func readMarketArchive(raw []byte, expectedDigest string) (archiveDocument, error) {
	var archive archiveDocument
	if len(raw) > MaxArchiveBytes || !utf8.Valid(raw) {
		return archive, errors.New("research archive exceeds its byte limit or has invalid UTF-8")
	}
	actual := sha256.Sum256(raw)
	if hex.EncodeToString(actual[:]) != expectedDigest {
		return archive, errors.New("original research archive SHA-256 mismatch")
	}
	body := bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
	if contract.Decode(body, nil, &archive) != nil || archive.Version != "0.1" ||
		(archive.SourceMode != "synthetic_fixture" && archive.SourceMode != "local_archive") || archive.Records == nil {
		return archive, errors.New("invalid original research archive envelope")
	}
	for _, record := range archive.Records {
		var fields struct {
			Kind     string `json:"kind"`
			Provider string `json:"provider"`
		}
		if json.Unmarshal(record, &fields) != nil || fields.Kind == "" || fields.Provider == "" {
			return archive, errors.New("invalid original research archive record")
		}
		if fields.Kind == outcomeKind || fields.Provider == outcomeProvider {
			return archive, errors.New("original research archive already contains a paper ledger record")
		}
	}
	return archive, nil
}
