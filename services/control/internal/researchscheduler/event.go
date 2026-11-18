package researchscheduler

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"time"
	"unicode/utf8"

	"tradingbot/control/internal/contract"
)

type EventReference struct {
	Kind     string `json:"kind"`
	Provider string `json:"provider"`
	RecordID string `json:"record_id"`
}

func (ref EventReference) archiveKind() string {
	switch ref.Kind {
	case "report":
		return "filing"
	case "news":
		return "news"
	case "price":
		return "bar"
	case "liquidity", "policy", "review":
		return "macro"
	default:
		return ""
	}
}

func (ref EventReference) valid() bool {
	return ref.archiveKind() != "" && identifier.MatchString(ref.Provider) && identifier.MatchString(ref.RecordID)
}

type eventAnchor struct {
	reference EventReference
	cutoff    time.Time
	symbols   []string
}

// VerifiedEvent is a point-in-time fact proved from the frozen, hashed archive.
// The zero value does not prove any event.
type VerifiedEvent struct {
	reference EventReference
	symbol    string
	eventTime time.Time
	knownAt   time.Time
	cutoff    time.Time
	digest    string
	payload   json.RawMessage
}

func (e VerifiedEvent) Kind() string          { return e.reference.Kind }
func (e VerifiedEvent) Provider() string      { return e.reference.Provider }
func (e VerifiedEvent) RecordID() string      { return e.reference.RecordID }
func (e VerifiedEvent) Symbol() string        { return e.symbol }
func (e VerifiedEvent) EventTime() time.Time  { return e.eventTime }
func (e VerifiedEvent) KnownAt() time.Time    { return e.knownAt }
func (e VerifiedEvent) Cutoff() time.Time     { return e.cutoff }
func (e VerifiedEvent) ArchiveSHA256() string { return e.digest }
func (e VerifiedEvent) Payload() []byte       { return bytes.Clone(e.payload) }

type archiveEvent struct {
	Kind        string          `json:"kind"`
	Provider    string          `json:"provider"`
	RecordID    string          `json:"record_id"`
	Symbol      json.RawMessage `json:"symbol"`
	Revision    int             `json:"revision"`
	EventTime   time.Time       `json:"event_time"`
	AvailableAt time.Time       `json:"available_at"`
	IngestedAt  time.Time       `json:"ingested_at"`
	Payload     json.RawMessage `json:"payload"`
}

// verify operates on the exact bytes hashed for the job. The trigger revision
// must also have been locally ingested by the portfolio cutoff.
func (a eventAnchor) verify(data []byte, digest string) (VerifiedEvent, error) {
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	var archive struct {
		Version string         `json:"schema_version"`
		Source  string         `json:"source_mode"`
		Records []archiveEvent `json:"records"`
	}
	if !utf8.Valid(data) || contract.Decode(data, nil, &archive) != nil || (archive.Version != "0.1" && archive.Version != "0.2") ||
		(archive.Source != "synthetic_fixture" && archive.Source != "local_archive") {
		return VerifiedEvent{}, errors.New("event requires a valid point-in-time archive")
	}
	var revisions []archiveEvent
	var eventSymbol string
	for _, record := range archive.Records {
		if record.Provider != a.reference.Provider || record.RecordID != a.reference.RecordID || record.AvailableAt.After(a.cutoff) {
			continue
		}
		var symbol *string
		if json.Unmarshal(record.Symbol, &symbol) != nil || symbol != nil && !slices.Contains(a.symbols, *symbol) {
			return VerifiedEvent{}, errors.New("event record is outside the requested universe")
		}
		name := ""
		if symbol != nil {
			name = *symbol
		}
		if !a.matches(record, name) || len(revisions) > 0 && eventSymbol != name {
			return VerifiedEvent{}, errors.New("event record identity is ambiguous or outside the requested universe")
		}
		eventSymbol = name
		revisions = append(revisions, record)
	}
	slices.SortFunc(revisions, func(a, b archiveEvent) int {
		switch {
		case a.Revision < b.Revision:
			return -1
		case a.Revision > b.Revision:
			return 1
		default:
			return 0
		}
	})
	for i, record := range revisions {
		if i > 0 && (record.Revision == revisions[i-1].Revision || record.AvailableAt.Before(revisions[i-1].AvailableAt)) {
			return VerifiedEvent{}, errors.New("event record revisions are inconsistent")
		}
	}
	if len(revisions) == 0 || revisions[len(revisions)-1].IngestedAt.After(a.cutoff) {
		return VerifiedEvent{}, errors.New("event record is unavailable at the portfolio cutoff")
	}
	latest := revisions[len(revisions)-1]
	return VerifiedEvent{
		reference: a.reference, symbol: eventSymbol, eventTime: latest.EventTime.UTC(), knownAt: latest.IngestedAt.UTC(),
		cutoff: a.cutoff.UTC(), digest: digest, payload: latest.Payload,
	}, nil
}

func (a eventAnchor) matches(record archiveEvent, symbol string) bool {
	kind := a.reference.archiveKind()
	if record.Kind != kind || (kind == "bar" && symbol == "") || record.Revision < 1 ||
		record.EventTime.IsZero() || record.AvailableAt.Before(record.EventTime) || record.IngestedAt.Before(record.AvailableAt) ||
		!bytes.HasPrefix(bytes.TrimSpace(record.Payload), []byte("{")) {
		return false
	}
	if kind != "macro" {
		return true
	}
	var payload struct {
		TriggerKind string `json:"trigger_kind"`
	}
	return json.Unmarshal(record.Payload, &payload) == nil && payload.TriggerKind == a.reference.Kind
}
