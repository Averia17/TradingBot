package paper_quotes

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"time"
	"unicode/utf8"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paper_sim"
)

const IdentityTapeVersion = "0.3"

const maxTapeBytes = 2 * 1024 * 1024

var (
	ErrQuoteNotInTape        = errors.New("paper quote bytes are absent from the owner-pinned tape")
	ErrQuoteUnavailable      = errors.New("paper quote is outside the tape's firm execution window")
	ErrAmbiguousHorizonQuote = errors.New("multiple distinct full-sale quotes at the latest horizon observation")
	tapeRef                  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,511}$`)
	horizonQuantity          = regexp.MustCompile(`^[1-9][0-9]{0,37}$`)
)

type tapeDocument struct {
	Version                string       `json:"paper_quote_tape_schema_version"`
	Source                 string       `json:"source_mode"`
	JobID                  string       `json:"job_id,omitempty"`
	AccountID              string       `json:"account_id,omitempty"`
	IdentityRegistrySHA256 string       `json:"identity_registry_sha256,omitempty"`
	Scope                  *tapeScope   `json:"scope,omitempty"`
	Observations           []tapeRecord `json:"observations"`
}

type tapeScope struct {
	Instrument  ledger.Instrument `json:"instrument"`
	Side        string            `json:"side"`
	QuantityRaw string            `json:"quantity_raw"`
}

type tapeRecord struct {
	AvailableAt time.Time       `json:"available_at"`
	Firmness    string          `json:"firmness"`
	Raw         json.RawMessage `json:"quote"`
}

// Tape contains the exact, immutable local observations verified at open.
type Tape struct {
	accountID              string
	identityRegistrySHA256 string
	digest                 string
	observations           []Observation
}

type Observation struct {
	AvailableAt time.Time
	Quote       paper_sim.Quote
	Raw         []byte
	SHA256      string
	firm        bool
}

func (t *Tape) AccountID() string              { return t.accountID }
func (t *Tape) IdentityRegistrySHA256() string { return t.identityRegistrySHA256 }
func (t *Tape) SHA256() string                 { return t.digest }

func OpenTape(path, digest, jobID, accountID string) (*Tape, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return openTape(root, filepath.Base(path), digest, jobID, accountID)
}

// OpenIdentityTape requires the independent owner pins for exact archive bytes,
// the virtual account and the security registry.
func OpenIdentityTape(root *os.Root, ref, digest, accountID, identityRegistrySHA256 string) (*Tape, error) {
	if root == nil || !tapeRef.MatchString(ref) || !fs.ValidPath(ref) || accountID == "" || identityRegistrySHA256 == "" {
		return nil, errors.New("invalid owner identity tape reference")
	}
	tape, err := openTape(root, ref, digest, "", accountID)
	if err != nil {
		return nil, err
	}
	if tape.IdentityRegistrySHA256() != identityRegistrySHA256 {
		return nil, errors.New("synthetic tape does not match the owner identity registry")
	}
	return tape, nil
}

func openTape(root *os.Root, ref, digest, jobID, accountID string) (*Tape, error) {
	info, err := root.Stat(ref)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxTapeBytes {
		return nil, errors.New("synthetic tape must be a bounded regular file")
	}
	file, err := root.Open(ref)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxTapeBytes+1))
	if err != nil {
		return nil, err
	}
	return DecodeTape(raw, digest, jobID, accountID)
}

func DecodeTape(raw []byte, digest, jobID, accountID string) (*Tape, error) {
	if len(raw) > maxTapeBytes || !utf8.Valid(raw) || sumHex(raw) != digest {
		return nil, errors.New("synthetic tape must be bounded UTF-8 with its exact lowercase SHA-256")
	}
	var doc tapeDocument
	if err := contract.Decode(raw, contract.PaperQuoteTape, &doc); err != nil {
		return nil, errors.New("invalid synthetic tape schema")
	}
	if doc.JobID != jobID || doc.AccountID != accountID {
		return nil, errors.New("synthetic tape does not match the requested job or account")
	}
	tape := &Tape{accountID: doc.AccountID, identityRegistrySHA256: doc.IdentityRegistrySHA256, digest: digest}
	for _, record := range doc.Observations {
		quote, err := paper_sim.ParseQuote(record.Raw)
		if err != nil {
			return nil, err
		}
		if quote.IdentityRegistrySHA256 != tape.identityRegistrySHA256 {
			return nil, errors.New("synthetic tape quote differs from its identity registry")
		}
		if record.AvailableAt.Before(quote.ObservedAt) || !quote.ObservedAt.Before(quote.ExpiresAt) {
			return nil, errors.New("synthetic tape observation, availability or expiry out of order")
		}
		if doc.Scope != nil && (quote.Instrument != doc.Scope.Instrument || quote.Side != doc.Scope.Side || quote.QuantityRaw != doc.Scope.QuantityRaw) {
			continue
		}
		tape.observations = append(tape.observations, Observation{
			AvailableAt: record.AvailableAt.UTC(), Quote: quote, Raw: record.Raw, SHA256: sumHex(record.Raw),
			firm: record.Firmness == "firm",
		})
	}
	slices.SortStableFunc(tape.observations, compareObservationPriority)
	return tape, nil
}

// Select finds the earliest eligible original observation approved for the
// specific decision. It returns no quote when nothing is currently usable.
func (t *Tape) Select(completedAt, now, deadline time.Time, authorize func([]byte, time.Time) error) *Observation {
	if completedAt.IsZero() || !now.Before(deadline) {
		return nil
	}
	first := sort.Search(len(t.observations), func(i int) bool {
		return t.observations[i].AvailableAt.After(completedAt)
	})
	// Check policy in selection order so a later quote cannot do work before
	// an earlier approved observation has been considered.
	for i := first; i < len(t.observations); i++ {
		row := &t.observations[i]
		if !row.AvailableAt.Before(now) {
			break
		}
		if !row.eligible(completedAt, now, deadline) || !now.Before(row.Quote.ExpiresAt) ||
			authorize(bytes.Clone(row.Raw), now) != nil {
			continue
		}
		selected := row.copy()
		return &selected
	}
	return nil
}

// HorizonExit values a full position at the cutoff using the latest firm sale
// observed in time and available then. Equal-time conflicting observations
// are ambiguous. The returned observation is evidence, never an execution.
func (t *Tape) HorizonExit(securityID string, instrument ledger.Instrument, quantityRaw string,
	cutoff time.Time, maxAge time.Duration) (Observation, bool, error) {
	if t == nil || t.accountID == "" || t.identityRegistrySHA256 == "" {
		return Observation{}, false, errors.New("horizon exit requires an account-bound identity tape")
	}
	if securityID == "" || instrument.ID == "" || instrument.RegistryVersion == "" ||
		instrument.Decimals < 0 || instrument.Decimals > 18 || !horizonQuantity.MatchString(quantityRaw) ||
		cutoff.IsZero() || maxAge <= 0 {
		return Observation{}, false, errors.New("invalid horizon exit identity, quantity or observation window")
	}

	var selected *Observation
	ambiguous := false
	for i := range t.observations {
		row := &t.observations[i]
		q := row.Quote
		if !row.firm || q.SecurityID != securityID || q.Instrument != instrument ||
			q.Side != "SELL" || q.QuantityRaw != quantityRaw || q.Execution != "fill" ||
			row.AvailableAt.After(cutoff) || !fresh(q.ObservedAt, q.ExpiresAt, cutoff, maxAge) {
			continue
		}
		fee, err := q.FeeUSD()
		if err != nil {
			return Observation{}, false, err
		}
		gross := decimal(q.GrossUSD)
		if gross.Sign() <= 0 || decimal(fee).Cmp(gross) > 0 {
			continue
		}

		switch {
		case selected == nil || q.ObservedAt.After(selected.Quote.ObservedAt):
			selected, ambiguous = row, false
		case q.ObservedAt.Equal(selected.Quote.ObservedAt):
			if !bytes.Equal(row.Raw, selected.Raw) {
				ambiguous = true
			} else if row.AvailableAt.Before(selected.AvailableAt) {
				selected = row
			}
		}
	}
	if ambiguous {
		return Observation{}, false, ErrAmbiguousHorizonQuote
	}
	if selected == nil {
		return Observation{}, false, nil
	}
	return selected.copy(), true, nil
}

// Verify proves exact membership and current execution eligibility, including
// a firm quote's recorded availability. The caller still checks policy and risk.
func (t *Tape) Verify(raw []byte, completedAt, now, deadline time.Time) (Observation, error) {
	return t.match(raw, completedAt, now, deadline, func(row Observation) bool {
		return now.Before(deadline) && now.Before(row.Quote.ExpiresAt)
	})
}

// VerifyPublished independently audits an immutable old pin. It can replay
// past expiry but may never be used to authorize a new reservation.
func (t *Tape) VerifyPublished(raw []byte, completedAt, now, deadline time.Time, authorize func([]byte, time.Time) error) (Observation, error) {
	return t.match(raw, completedAt, now, deadline, func(row Observation) bool {
		return authorize(bytes.Clone(row.Raw), row.AvailableAt) == nil
	})
}

func (t *Tape) match(raw []byte, completedAt, now, deadline time.Time, authorize func(Observation) bool) (Observation, error) {
	found := false
	for i := range t.observations {
		row := &t.observations[i]
		if !bytes.Equal(row.Raw, raw) {
			continue
		}
		found = true
		if row.eligible(completedAt, now, deadline) && authorize(*row) {
			return row.copy(), nil
		}
	}
	if found {
		return Observation{}, ErrQuoteUnavailable
	}
	return Observation{}, ErrQuoteNotInTape
}

func (o Observation) eligible(completedAt, now, deadline time.Time) bool {
	return o.firm && !completedAt.IsZero() && o.Quote.ObservedAt.After(completedAt) && o.AvailableAt.After(completedAt) &&
		o.AvailableAt.Before(now) && o.AvailableAt.Before(deadline) && o.AvailableAt.Before(o.Quote.ExpiresAt)
}

func compareObservationPriority(a, b Observation) int {
	if byAvailability := a.AvailableAt.Compare(b.AvailableAt); byAvailability != 0 {
		return byAvailability
	}
	if byObservation := a.Quote.ObservedAt.Compare(b.Quote.ObservedAt); byObservation != 0 {
		return byObservation
	}
	return cmp.Compare(a.SHA256, b.SHA256)
}

func (o Observation) copy() Observation {
	o.Raw = bytes.Clone(o.Raw)
	if o.Quote.FeeBPS != nil {
		o.Quote.FeeBPS = new(*o.Quote.FeeBPS)
	}
	return o
}

func sumHex(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}
