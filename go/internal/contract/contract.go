// Package contract validates our research wire protocol, never execution authority.
package contract

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed schema.json
var batchSchema []byte

//go:embed portfolio.schema.json
var portfolioSchema []byte

var decimalPattern = regexp.MustCompile(`^(?:0|[1-9][0-9]{0,11})(?:\.[0-9]{1,8})?$`)

type Horizon struct {
	Min int `json:"min_hours"`
	Max int `json:"max_hours"`
}
type Intent struct {
	Symbol       string   `json:"symbol"`
	Action       string   `json:"action"`
	Notional     string   `json:"notional_usd"`
	Confidence   *float64 `json:"confidence"`
	Expected     *float64 `json:"expected_return_pct"`
	Horizon      Horizon  `json:"horizon"`
	Thesis       []string `json:"thesis"`
	Invalidation []string `json:"invalidation_conditions"`
	Evidence     []string `json:"evidence_ids"`
	CostBPS      int      `json:"max_execution_cost_bps"`
	ID           string   `json:"decision_id"`
}
type Batch struct {
	Version    string            `json:"schema_version"`
	Mode       string            `json:"execution_mode"`
	RunID      string            `json:"run_id"`
	SnapshotID string            `json:"portfolio_snapshot_id"`
	AsOf       time.Time         `json:"as_of"`
	Created    time.Time         `json:"created_at"`
	Expires    time.Time         `json:"expires_at"`
	Evidence   map[string]string `json:"evidence_hashes"`
	Intents    []Intent          `json:"intents"`
}
type Position struct {
	Symbol     string `json:"symbol"`
	Instrument string `json:"instrument_id"`
	Quantity   string `json:"quantity"`
	Mark       string `json:"mark_price_usd"`
}
type Portfolio struct {
	ID          string     `json:"snapshot_id"`
	AsOf        time.Time  `json:"as_of"`
	Currency    string     `json:"currency"`
	CashAsset   string     `json:"cash_asset"`
	Cash        string     `json:"cash_usd"`
	Reserved    string     `json:"reserved_cash_usd"`
	MaxPosition string     `json:"max_position_pct"`
	Positions   []Position `json:"positions"`
}

func compile(data []byte) *jsonschema.Schema {
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		panic(err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err = c.AddResource("https://tradingbot.local/schema", v); err != nil {
		panic(err)
	}
	s, err := c.Compile("https://tradingbot.local/schema")
	if err != nil {
		panic(err)
	}
	return s
}

var batchValidator = compile(batchSchema)
var portfolioValidator = compile(portfolioSchema)

// Decode rejects duplicate keys, trailing values and unknown fields. Numbers
// remain json.Number during schema validation; monetary fields must be strings.
func Decode(data []byte, validator *jsonschema.Schema, out any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := walk(d); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	if validator != nil {
		value, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			return err
		}
		if err = validator.Validate(value); err != nil {
			return errors.New("schema violation")
		}
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(out)
}
func walk(d *json.Decoder) error {
	tok, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	if delim != '{' && delim != '[' {
		return errors.New("unexpected delimiter")
	}
	keys := map[string]bool{}
	for d.More() {
		if delim == '{' {
			k, err := d.Token()
			if err != nil {
				return err
			}
			key, ok := k.(string)
			if !ok || keys[key] {
				return errors.New("duplicate JSON key")
			}
			keys[key] = true
		}
		if err := walk(d); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}

func Amount(s string) (*big.Rat, error) {
	if !decimalPattern.MatchString(s) {
		return nil, errors.New("invalid exact decimal")
	}
	n, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, errors.New("invalid amount")
	}
	return n, nil
}
func ParseBatch(data []byte, now time.Time, maxAge time.Duration) (Batch, error) {
	var b Batch
	if err := Decode(data, batchValidator, &b); err != nil {
		return b, err
	}
	if b.Version != "0.1" || b.Mode != "research" {
		return b, errors.New("unsupported protocol")
	}
	if b.AsOf.After(b.Created) || b.Created.After(now) || !b.Expires.After(now) || !b.Expires.After(b.Created) || now.Sub(b.AsOf) > maxAge {
		return b, errors.New("expired, stale or future batch")
	}
	symbols, ids := map[string]bool{}, map[string]bool{}
	for _, i := range b.Intents {
		if symbols[i.Symbol] || ids[i.ID] {
			return b, errors.New("duplicate decision")
		}
		symbols[i.Symbol] = true
		ids[i.ID] = true
		amount, err := Amount(i.Notional)
		if err != nil {
			return b, err
		}
		if i.Horizon.Min > i.Horizon.Max {
			return b, errors.New("invalid horizon")
		}
		if i.Action == "HOLD" {
			if amount.Sign() != 0 {
				return b, errors.New("HOLD amount")
			}
		} else if amount.Sign() <= 0 || i.Confidence == nil || i.Expected == nil || len(i.Evidence) == 0 {
			return b, errors.New("incomplete intent")
		}
		for _, ref := range i.Evidence {
			if _, ok := b.Evidence[ref]; !ok {
				return b, errors.New("unknown evidence")
			}
		}
	}
	return b, nil
}
func ParsePortfolio(data []byte) (Portfolio, error) {
	var p Portfolio
	if err := Decode(data, portfolioValidator, &p); err != nil {
		return p, err
	}
	cash, err := Amount(p.Cash)
	if err != nil {
		return p, err
	}
	reserved, err := Amount(p.Reserved)
	if err != nil {
		return p, err
	}
	cap, ok := new(big.Rat).SetString(p.MaxPosition)
	if !ok || cap.Sign() <= 0 || cap.Cmp(big.NewRat(1, 1)) > 0 || reserved.Cmp(cash) > 0 {
		return p, errors.New("invalid portfolio limits")
	}
	seen := map[string]bool{}
	for _, pos := range p.Positions {
		if seen[pos.Symbol] {
			return p, errors.New("duplicate position")
		}
		seen[pos.Symbol] = true
		for _, s := range []string{pos.Quantity, pos.Mark} {
			n, err := Amount(s)
			if err != nil || n.Sign() <= 0 {
				return p, errors.New("invalid position amount")
			}
		}
	}
	return p, nil
}

// CheckAllocation is an exact research precheck against a control-owned snapshot.
// It does not reserve cash and cannot authorize paper or live orders.
func CheckAllocation(b Batch, p Portfolio, symbols []string) error {
	if b.SnapshotID != p.ID || !b.AsOf.Equal(p.AsOf) {
		return errors.New("snapshot mismatch")
	}
	requested := map[string]bool{}
	for _, s := range symbols {
		if requested[s] {
			return errors.New("duplicate requested symbol")
		}
		requested[s] = true
	}
	if len(requested) != len(b.Intents) {
		return errors.New("symbol count mismatch")
	}
	cash, _ := Amount(p.Cash)
	reserve, _ := Amount(p.Reserved)
	equity := new(big.Rat).Set(cash)
	holdings := map[string]*big.Rat{}
	for _, pos := range p.Positions {
		q, _ := Amount(pos.Quantity)
		m, _ := Amount(pos.Mark)
		v := new(big.Rat).Mul(q, m)
		holdings[pos.Symbol] = v
		equity.Add(equity, v)
	}
	cap, _ := new(big.Rat).SetString(p.MaxPosition)
	limit := new(big.Rat).Mul(equity, cap)
	buys := new(big.Rat)
	for _, i := range b.Intents {
		if !requested[i.Symbol] {
			return errors.New("unrequested symbol")
		}
		held := holdings[i.Symbol]
		n, _ := Amount(i.Notional)
		switch i.Action {
		case "HOLD":
			continue
		case "BUY":
			if held != nil {
				return errors.New("BUY requires unheld asset")
			}
		case "ADD", "SELL", "REDUCE":
			if held == nil {
				return errors.New("missing held instrument")
			}
		}
		if i.Action == "BUY" || i.Action == "ADD" {
			existing := new(big.Rat)
			if held != nil {
				existing.Set(held)
			}
			if existing.Add(existing, n).Cmp(limit) > 0 {
				return errors.New("concentration exceeded")
			}
			buys.Add(buys, n)
		}
		if i.Action == "SELL" && n.Cmp(held) != 0 {
			return errors.New("SELL must close position")
		}
		if i.Action == "REDUCE" && n.Cmp(held) >= 0 {
			return errors.New("REDUCE must be partial")
		}
	}
	if buys.Cmp(new(big.Rat).Sub(cash, reserve)) > 0 {
		return errors.New("cash exceeded")
	}
	return nil
}

func CheckSymbols(symbols []string) error {
	valid := regexp.MustCompile(`^[A-Z][A-Z0-9.\-]{0,15}$`)
	seen := map[string]bool{}
	if len(symbols) == 0 || len(symbols) > 100 {
		return errors.New("invalid symbol count")
	}
	for _, s := range symbols {
		if !valid.MatchString(s) || seen[s] {
			return fmt.Errorf("invalid symbol %s", strings.TrimSpace(s))
		}
		seen[s] = true
	}
	return nil
}
