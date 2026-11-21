package paper_memory

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
	"tradingbot/control/internal/paper_risk"
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
	DurableMemory    *durableMemory    `json:"durable_memory,omitempty"`
}

type durableMemory struct {
	AsOf   time.Time      `json:"as_of"`
	Active *durableLesson `json:"active"`
}

type durableLesson struct {
	LessonID            string    `json:"lesson_id"`
	EventID             string    `json:"event_id"`
	Revision            int       `json:"revision"`
	Action              string    `json:"action"`
	AvailableAt         time.Time `json:"available_at"`
	SourceOutcomeSHA256 string    `json:"source_outcome_sha256"`
}

// ExportArchive appends recent realized outcomes from the ledger prefix to a
// hash-verified local archive. An existing archive cannot supply ledger records.
func ExportArchive(ctx context.Context, reader EventReader, accountID string, asOf time.Time, policy paper_risk.Policy, raw []byte, expectedDigest string) ([]byte, error) {
	return exportArchive(ctx, reader, nil, accountID, asOf, policy, raw, expectedDigest)
}

// ExportArchiveWithLessons attaches the latest visible lesson from the same
// consistent SQL snapshot as the economic facts. The frozen cutoff belongs
// to the lesson selection and does not delay the underlying accounting.
func ExportArchiveWithLessons(ctx context.Context, snapshot ledger.ReadSnapshot, accountID string, asOf time.Time, policy paper_risk.Policy, raw []byte, expectedDigest string) ([]byte, error) {
	if asOf.IsZero() || asOf.After(snapshot.Cutoff()) || asOf.Nanosecond()%1_000 != 0 {
		return nil, ledger.ErrInvalid
	}
	return exportArchive(ctx, snapshot, snapshot.LessonForFill, accountID, asOf, policy, raw, expectedDigest)
}

type lessonLookup func(context.Context, string, string, time.Time) (ledger.LessonEvent, error)

func exportArchive(ctx context.Context, reader EventReader, lessons lessonLookup, accountID string, asOf time.Time, policy paper_risk.Policy, raw []byte, expectedDigest string) ([]byte, error) {
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
	rules := make(map[ledger.Instrument]paper_risk.InstrumentRule, len(policy.Instruments))
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
		if lessons != nil {
			memory, err := selectLesson(ctx, lessons, accountID, asOf, outcome)
			if err != nil {
				return nil, err
			}
			record.Payload.SchemaVersion = "0.2"
			record.Payload.DurableMemory = &memory
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

func selectLesson(ctx context.Context, lookup lessonLookup, accountID string, asOf time.Time, outcome Outcome) (durableMemory, error) {
	memory := durableMemory{AsOf: asOf.UTC()}
	event, err := lookup(ctx, accountID, outcome.FillEventID, asOf)
	if errors.Is(err, ledger.ErrNotFound) {
		return memory, nil
	}
	if err != nil {
		return memory, err
	}
	expected, err := lessonFact(accountID, outcome)
	if err != nil {
		return memory, err
	}
	if event.Fact != expected || event.AvailableAt.Before(outcome.AvailableAt) || event.AvailableAt.After(asOf) {
		return memory, errors.New("stored paper lesson differs from its verified FIFO outcome or cutoff")
	}
	switch event.Action {
	case "retract":
		return memory, nil
	case "record", "reinstate":
		memory.Active = &durableLesson{
			LessonID: event.LessonID, EventID: event.EventID, Revision: event.Revision, Action: event.Action,
			AvailableAt: event.AvailableAt.UTC(), SourceOutcomeSHA256: event.Fact.SourceOutcomeSHA256,
		}
		return memory, nil
	default:
		return memory, errors.New("invalid paper lesson activation")
	}
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
