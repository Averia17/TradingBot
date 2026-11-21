// Package quote_collect turns local HTTP fixture responses into a synthetic paper tape.
package quote_collect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
	"unicode/utf8"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paper_quotes"
	"tradingbot/control/internal/paper_sim"
)

const Version = "0.1"
const MaxConfigBytes = 256 * 1024

const cashDecimals = 6
const maxCashRawDigits = 30 + cashDecimals // Paper ledger allows 30 USD integer digits.

type Config struct {
	Version                string    `json:"paper_quote_collection_schema_version"`
	AccountID              string    `json:"account_id"`
	IdentityRegistrySHA256 string    `json:"identity_registry_sha256"`
	CashMint               string    `json:"cash_mint"`
	Requests               []Request `json:"requests"`
}

type Request struct {
	SecurityID string            `json:"security_id"`
	Instrument ledger.Instrument `json:"instrument"`
	TokenMint  string            `json:"token_mint"`
	Side       string            `json:"side"`
	AmountRaw  string            `json:"amount_raw"`
}

type Tape struct {
	Raw    []byte
	SHA256 string
}

type tapeDocument struct {
	Version                string        `json:"paper_quote_tape_schema_version"`
	Source                 string        `json:"source_mode"`
	AccountID              string        `json:"account_id"`
	IdentityRegistrySHA256 string        `json:"identity_registry_sha256"`
	Observations           []observation `json:"observations"`
}

type observation struct {
	AvailableAt time.Time       `json:"available_at"`
	Firmness    string          `json:"firmness"`
	Quote       paper_sim.Quote `json:"quote"`
}

func ParseConfig(raw []byte) (Config, error) {
	var config Config
	if len(raw) > MaxConfigBytes || !utf8.Valid(raw) || contract.Decode(raw, contract.PaperQuoteCollectionConfig, &config) != nil {
		return Config{}, errors.New("invalid synthetic quote collection request")
	}
	for i, row := range config.Requests {
		if row.TokenMint == config.CashMint || (row.Side == "BUY" && len(row.AmountRaw) > maxCashRawDigits) {
			return Config{}, fmt.Errorf("quote request %d has identical mints or exceeds paper USD precision", i+1)
		}
	}
	return config, nil
}

// Collect validates every fixture response before returning a complete tape.
// A nonzero asOf sets the clock for fixture replay. Otherwise freshness is
// checked against the time each response is received.
func Collect(ctx context.Context, endpoint string, config Config, asOf time.Time) (Tape, error) {
	rawConfig, err := json.Marshal(config)
	if err != nil {
		return Tape{}, err
	}
	config, err = ParseConfig(rawConfig)
	if err != nil {
		return Tape{}, err
	}
	source, err := openFixtureSource(endpoint)
	if err != nil {
		return Tape{}, err
	}
	defer source.client.CloseIdleConnections()

	doc := tapeDocument{
		Version: paper_quotes.IdentityTapeVersion, Source: paper_sim.Source, AccountID: config.AccountID,
		IdentityRegistrySHA256: config.IdentityRegistrySHA256, Observations: make([]observation, 0, len(config.Requests)),
	}
	for i, row := range config.Requests {
		request := fixtureRequest{Version: Version, InputMint: config.CashMint, OutputMint: row.TokenMint, InAmount: row.AmountRaw}
		if row.Side == "SELL" {
			request.InputMint, request.OutputMint = request.OutputMint, request.InputMint
		}
		response, err := source.fetch(ctx, request)
		if err != nil {
			return Tape{}, fmt.Errorf("collect fixture quote %d: %w", i+1, err)
		}
		checkedAt := asOf
		if checkedAt.IsZero() {
			checkedAt = time.Now().UTC()
		}
		collected, err := normalize(row, config.IdentityRegistrySHA256, request, response, checkedAt)
		if err != nil {
			return Tape{}, fmt.Errorf("collect fixture quote %d: %w", i+1, err)
		}
		doc.Observations = append(doc.Observations, collected)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return Tape{}, err
	}
	digest := sha256.Sum256(raw)
	checksum := hex.EncodeToString(digest[:])
	if _, err := paper_quotes.DecodeTape(raw, checksum, "", config.AccountID); err != nil {
		return Tape{}, fmt.Errorf("validate collected paper tape: %w", err)
	}
	return Tape{Raw: raw, SHA256: checksum}, nil
}

func normalize(row Request, registry string, request fixtureRequest, reply fixtureResponse, asOf time.Time) (observation, error) {
	if reply.fixtureRequest != request {
		return observation{}, errors.New("fixture response mint pair or exact input differs from the request")
	}
	if len(reply.MinOutAmount) > len(reply.OutAmount) ||
		(len(reply.MinOutAmount) == len(reply.OutAmount) && reply.MinOutAmount > reply.OutAmount) {
		return observation{}, errors.New("fixture minimum output exceeds quoted output")
	}
	if reply.ObservedAt.IsZero() || reply.AvailableAt.Before(reply.ObservedAt) ||
		reply.AvailableAt.After(asOf) || !asOf.Before(reply.ExpiresAt) {
		return observation{}, errors.New("fixture quote observation, availability, or expiry is outside the collection window")
	}
	rawCash, quantity := reply.InAmount, reply.OutAmount
	if row.Side == "SELL" {
		rawCash, quantity = reply.OutAmount, reply.InAmount
	}
	if len(rawCash) > maxCashRawDigits {
		return observation{}, errors.New("fixture cash amount exceeds paper USD precision")
	}
	quote := paper_sim.Quote{
		Version: paper_sim.IdentityQuoteVersion, Source: paper_sim.Source, Instrument: row.Instrument,
		Side: row.Side, QuantityRaw: quantity, GrossUSD: cashUSD(rawCash), FeeBPS: &reply.FeeBPS,
		FixedFeeUSD: reply.FixedFeeUSD, ObservedAt: reply.ObservedAt.UTC(), ExpiresAt: reply.ExpiresAt.UTC(),
		Execution: "fill", SecurityID: row.SecurityID, IdentityRegistrySHA256: registry,
	}
	feeUSD, err := quote.FeeUSD()
	if err != nil {
		return observation{}, err
	}
	if row.Side == "SELL" {
		fee, _ := new(big.Rat).SetString(feeUSD)
		gross, _ := new(big.Rat).SetString(quote.GrossUSD)
		if fee.Cmp(gross) > 0 {
			return observation{}, errors.New("fixture sale fee exceeds quoted proceeds")
		}
	}
	return observation{AvailableAt: reply.AvailableAt.UTC(), Firmness: "firm", Quote: quote}, nil
}

// cashUSD maps six-decimal fixture USDC to paper USD at par without rounding.
func cashUSD(raw string) string {
	if len(raw) <= cashDecimals {
		raw = strings.Repeat("0", cashDecimals+1-len(raw)) + raw
	}
	decimalAt := len(raw) - cashDecimals
	return strings.TrimRight(strings.TrimRight(raw[:decimalAt]+"."+raw[decimalAt:], "0"), ".")
}
