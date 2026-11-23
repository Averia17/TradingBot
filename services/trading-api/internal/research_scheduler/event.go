package research_scheduler

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"time"
	"unicode/utf8"

	"tradingbot/trading-api/internal/contract"
)

const maxArchiveBytes = 16 * 1024 * 1024

var (
	identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	localRef   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,511}$`)
	digest     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type ArchiveReference struct {
	Ref    string `json:"ref"`
	SHA256 string `json:"sha256"`
}

func (ref ArchiveReference) valid() bool {
	return localRef.MatchString(ref.Ref) && fs.ValidPath(ref.Ref) && digest.MatchString(ref.SHA256)
}

type archiveProof struct {
	reference ArchiveReference
	event     *eventLookup
}

// VerifyArchive proves the bytes of a pinned local research archive. An event
// additionally requires its archived record to be visible at the cutoff.
func VerifyArchive(archiveRoot string, reference ArchiveReference, cutoff time.Time, symbols []string, event *EventReference) (VerifiedEvent, error) {
	if !reference.valid() || cutoff.IsZero() || contract.CheckSymbols(symbols) != nil {
		return VerifiedEvent{}, errors.New("invalid research archive reference or cutoff")
	}
	proof := archiveProof{reference: reference}
	if event != nil {
		if !event.valid() {
			return VerifiedEvent{}, errors.New("invalid archive event reference")
		}
		proof.event = &eventLookup{reference: *event, cutoff: cutoff, symbols: symbols}
	}
	return proof.verifyLocal(archiveRoot)
}

func (proof archiveProof) verifyLocal(path string) (VerifiedEvent, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return VerifiedEvent{}, fmt.Errorf("open local archive root: %w", err)
	}
	defer root.Close()
	return proof.verify(root, true)
}

// verify always rechecks the path and digest. After a successful event proof,
// a matching digest proves the retry is using the same validated archive.
func (proof archiveProof) verify(root *os.Root, checkEvent bool) (VerifiedEvent, error) {
	reference := proof.reference
	info, err := root.Stat(reference.Ref)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxArchiveBytes {
		return VerifiedEvent{}, errors.New("archive is missing, oversized or outside the configured root")
	}
	file, err := root.Open(reference.Ref)
	if err != nil {
		return VerifiedEvent{}, errors.New("cannot open local archive")
	}
	defer file.Close()
	hasher := sha256.New()
	var archiveJSON bytes.Buffer
	var writer io.Writer = hasher
	readEvent := checkEvent && proof.event != nil
	if readEvent {
		archiveJSON.Grow(int(info.Size()))
		writer = io.MultiWriter(hasher, &archiveJSON)
	}
	n, err := io.Copy(writer, io.LimitReader(file, maxArchiveBytes+1))
	if err != nil || n > maxArchiveBytes || fmt.Sprintf("%x", hasher.Sum(nil)) != reference.SHA256 {
		return VerifiedEvent{}, errors.New("cannot verify local archive SHA-256")
	}
	if readEvent {
		return proof.event.verify(archiveJSON.Bytes(), reference.SHA256)
	}
	return VerifiedEvent{}, nil
}

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

type eventLookup struct {
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
func (a eventLookup) verify(data []byte, digest string) (VerifiedEvent, error) {
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
		return cmp.Compare(a.Revision, b.Revision)
	})
	for i := 1; i < len(revisions); i++ {
		record, prior := revisions[i], revisions[i-1]
		if record.Revision == prior.Revision || record.AvailableAt.Before(prior.AvailableAt) {
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

func (a eventLookup) matches(record archiveEvent, symbol string) bool {
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
