package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
	"unicode/utf8"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paperapply"
	"tradingbot/control/internal/papersim"
)

const maxTapeBytes = 2 * 1024 * 1024

type quoteTape struct {
	Version      string        `json:"paper_quote_tape_schema_version"`
	Source       string        `json:"source_mode"`
	JobID        string        `json:"job_id"`
	Scope        tapeScope     `json:"scope"`
	Observations []observation `json:"observations"`
}

type tapeScope struct {
	Instrument  ledger.Instrument `json:"instrument"`
	Side        string            `json:"side"`
	QuantityRaw string            `json:"quantity_raw"`
}

type observation struct {
	Available time.Time       `json:"available_at"`
	Firmness  string          `json:"firmness"`
	Raw       json.RawMessage `json:"quote"`
}

type tapeCandidate struct {
	available time.Time
	quote     papersim.Quote
	raw       []byte
	digest    string
}

func decodeTape(raw []byte, digest, jobID string) (quoteTape, error) {
	var tape quoteTape
	if len(raw) > maxTapeBytes || !utf8.Valid(raw) || sha256Hex(raw) != digest {
		return tape, errors.New("synthetic tape must be bounded UTF-8 with its exact lowercase SHA-256")
	}
	if err := contract.Decode(raw, contract.PaperQuoteTape, &tape); err != nil || tape.JobID != jobID {
		return tape, errors.New("invalid synthetic tape schema or job ID")
	}
	return tape, nil
}

func (t quoteTape) eligible(obs observation, completedAt, now, deadline time.Time) (tapeCandidate, bool, error) {
	if len(obs.Raw) > papersim.MaxInputBytes {
		return tapeCandidate{}, false, errors.New("synthetic tape quote exceeds the fixture size limit")
	}
	quote, err := papersim.ParseQuote(obs.Raw)
	if err != nil {
		return tapeCandidate{}, false, err
	}
	if obs.Available.Before(quote.ObservedAt) || !quote.ObservedAt.Before(quote.ExpiresAt) {
		return tapeCandidate{}, false, errors.New("synthetic tape observation, availability or expiry out of order")
	}
	if obs.Firmness != "firm" || quote.Instrument != t.Scope.Instrument || quote.Side != t.Scope.Side ||
		quote.QuantityRaw != t.Scope.QuantityRaw || !quote.ObservedAt.After(completedAt) || !obs.Available.After(completedAt) ||
		!obs.Available.Before(now) || !obs.Available.Before(deadline) || !obs.Available.Before(quote.ExpiresAt) {
		return tapeCandidate{}, false, nil
	}
	return tapeCandidate{obs.Available, quote, obs.Raw, sha256Hex(obs.Raw)}, true, nil
}

func (t quoteTape) selectQuote(review paperapply.QuoteReview, completedAt, now time.Time) (*tapeCandidate, error) {
	var chosen *tapeCandidate
	deadline := review.ExpiresAt()
	for _, obs := range t.Observations {
		candidate, usable, err := t.eligible(obs, completedAt, now, deadline)
		if err != nil {
			return nil, err
		}
		if !usable || !now.Before(candidate.quote.ExpiresAt) || (chosen != nil && !candidate.earlier(*chosen)) {
			continue
		}
		if err := review.Authorize(candidate.raw, now); err != nil {
			continue
		}
		chosen = &candidate
	}
	return chosen, nil
}

func (t quoteTape) replay(root *os.Root, pin quotePin, tapeDigest string, review paperapply.QuoteReview, completedAt, now time.Time) (tapeCandidate, error) {
	conflict := fmt.Errorf("%w: existing paper quote pin differs from the bound tape", ledger.ErrConflict)
	if pin.Quote.Ref != quoteFilename(tapeDigest, pin.Quote.SHA256) {
		return tapeCandidate{}, conflict
	}
	body, err := readRoot(root, pin.Quote.Ref, papersim.MaxInputBytes)
	if err != nil {
		return tapeCandidate{}, fmt.Errorf("read existing pinned quote: %w", err)
	}
	var matched *tapeCandidate
	for _, obs := range t.Observations {
		if !bytes.Equal(obs.Raw, body) {
			continue
		}
		candidate, usable, err := t.eligible(obs, completedAt, now, review.ExpiresAt())
		if err != nil {
			return tapeCandidate{}, err
		}
		if usable && candidate.reference(tapeDigest) == pin.Quote && review.Authorize(candidate.raw, candidate.available) == nil &&
			(matched == nil || candidate.earlier(*matched)) {
			matched = &candidate
		}
	}
	if matched == nil {
		return tapeCandidate{}, conflict
	}
	return *matched, nil
}

func (c tapeCandidate) earlier(other tapeCandidate) bool {
	if !c.available.Equal(other.available) {
		return c.available.Before(other.available)
	}
	if !c.quote.ObservedAt.Equal(other.quote.ObservedAt) {
		return c.quote.ObservedAt.Before(other.quote.ObservedAt)
	}
	return c.digest < other.digest
}

func (c tapeCandidate) reference(tapeDigest string) pinQuote {
	return pinQuote{Ref: quoteFilename(tapeDigest, c.digest), SHA256: c.digest}
}

func quoteFilename(tapeDigest, quoteDigest string) string {
	return "quote-" + tapeDigest + "-" + quoteDigest + ".json"
}

func sha256Hex(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}
