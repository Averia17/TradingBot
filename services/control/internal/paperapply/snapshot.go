package paperapply

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"time"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paperrisk"
)

var (
	uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)
	rawPattern  = regexp.MustCompile(`^(?:0|[1-9][0-9]{0,37})$`)
)

// SnapshotContext carries the owner policy and point-in-time marks used to
// convert the versioned paper book into a research portfolio.
type SnapshotContext struct {
	AccountID       string           `json:"account_id"`
	SnapshotVersion int64            `json:"snapshot_version"`
	Policy          paperrisk.Policy `json:"policy"`
	Marks           []paperrisk.Mark `json:"marks"`
}

// CheckMarks validates that the bound marks are still usable at a later cutoff.
func (c SnapshotContext) CheckMarks(at time.Time) error {
	_, _, err := c.inputs(at)
	return err
}

// CheckPortfolio verifies a stored portfolio's binding to the versioned account,
// policy and available marks without requiring the account to still be current.
func (c SnapshotContext) CheckPortfolio(portfolio contract.Portfolio) error {
	if err := c.CheckMarks(portfolio.AsOf); err != nil {
		return err
	}
	id, err := c.snapshotID(portfolio)
	if err != nil || id != portfolio.ID {
		return fmt.Errorf("%w: portfolio is not bound to paper snapshot and policy", ErrInvalid)
	}
	return nil
}

// Snapshot projects a paper account into the wire format used by research.
// Amounts outside the research contract's exact range require a new contract.
func Snapshot(account ledger.Account, context SnapshotContext, asOf time.Time) (contract.Portfolio, error) {
	rules, marks, err := context.inputs(asOf)
	if err != nil {
		return contract.Portfolio{}, err
	}
	if account.ID != context.AccountID || account.Version != context.SnapshotVersion {
		return contract.Portfolio{}, ledger.ErrStale
	}
	if account.CreatedAt.IsZero() || account.CreatedAt.After(asOf) || len(account.Positions) > len(rules) {
		return contract.Portfolio{}, fmt.Errorf("%w: account was unavailable at snapshot time", ErrInvalid)
	}
	portfolio := contract.Portfolio{
		AsOf: asOf.UTC(), Currency: "USD", CashAsset: "USD", Cash: account.CashUSD, Reserved: account.ReservedCashUSD,
		MaxPosition: big.NewRat(int64(context.Policy.MaxPositionBPS), 10000).FloatString(4),
		Positions:   make([]contract.Position, 0, len(account.Positions)),
	}
	for _, holding := range account.Positions {
		rule, enabled := rules[holding.Instrument]
		mark, marked := marks[holding.Instrument]
		units, err := rawAmount(holding.QuantityRaw, holding.Decimals)
		reserved, reserveErr := rawAmount(holding.ReservedQuantityRaw, holding.Decimals)
		if !enabled || !marked || err != nil || reserveErr != nil || reserved.Cmp(units) > 0 {
			return contract.Portfolio{}, fmt.Errorf("%w: unvalued or invalid paper position", ErrInvalid)
		}
		quantity := units.FloatString(8)
		exact, err := contract.Amount(quantity)
		if err != nil || exact.Cmp(units) != 0 {
			return contract.Portfolio{}, fmt.Errorf("%w: paper position cannot be represented exactly in research", ErrInvalid)
		}
		portfolio.Positions = append(portfolio.Positions, contract.Position{
			Symbol: rule.Symbol, Instrument: rule.Instrument.ID, Quantity: quantity, Mark: mark.PriceUSD,
		})
	}
	sort.Slice(portfolio.Positions, func(i, j int) bool { return portfolio.Positions[i].Symbol < portfolio.Positions[j].Symbol })
	portfolio.ID, err = context.snapshotID(portfolio)
	if err != nil {
		return contract.Portfolio{}, err
	}
	body, err := json.Marshal(portfolio)
	if err != nil {
		return contract.Portfolio{}, err
	}
	if _, err := contract.ParsePortfolio(body); err != nil {
		return contract.Portfolio{}, fmt.Errorf("%w: invalid paper research portfolio: %v", ErrInvalid, err)
	}
	return portfolio, nil
}

func (c SnapshotContext) inputs(at time.Time) (map[ledger.Instrument]paperrisk.InstrumentRule, map[ledger.Instrument]paperrisk.Mark, error) {
	if !uuidPattern.MatchString(c.AccountID) || c.SnapshotVersion < 1 || at.IsZero() {
		return nil, nil, fmt.Errorf("%w: invalid paper snapshot identity", ErrInvalid)
	}
	if err := c.Policy.Validate(); err != nil {
		return nil, nil, err
	}
	if len(c.Marks) == 0 || len(c.Marks) > len(c.Policy.Instruments) {
		return nil, nil, fmt.Errorf("%w: invalid paper mark count", ErrInvalid)
	}
	rules := make(map[ledger.Instrument]paperrisk.InstrumentRule, len(c.Policy.Instruments))
	for _, rule := range c.Policy.Instruments {
		rules[rule.Instrument] = rule
	}
	marks := make(map[ledger.Instrument]paperrisk.Mark, len(c.Marks))
	maxAge := time.Duration(c.Policy.MaxMarkAgeSeconds) * time.Second
	for _, mark := range c.Marks {
		_, enabled := rules[mark.Instrument]
		_, duplicate := marks[mark.Instrument]
		price, err := contract.Amount(mark.PriceUSD)
		if !enabled || duplicate || err != nil || price.Sign() <= 0 || mark.At.IsZero() ||
			mark.AvailableAt.Before(mark.At) || mark.AvailableAt.After(at) || at.Sub(mark.At) > maxAge {
			return nil, nil, fmt.Errorf("%w: invalid or stale paper mark", ErrInvalid)
		}
		marks[mark.Instrument] = mark
	}
	return rules, marks, nil
}

func (c SnapshotContext) snapshotID(portfolio contract.Portfolio) (string, error) {
	portfolio.ID, portfolio.AsOf = "", portfolio.AsOf.UTC()
	c.Policy.Instruments = append([]paperrisk.InstrumentRule(nil), c.Policy.Instruments...)
	c.Marks = append([]paperrisk.Mark(nil), c.Marks...)
	for i := range c.Marks {
		c.Marks[i].At, c.Marks[i].AvailableAt = c.Marks[i].At.UTC(), c.Marks[i].AvailableAt.UTC()
	}
	sort.Slice(c.Policy.Instruments, func(i, j int) bool {
		return c.Policy.Instruments[i].Instrument.ID < c.Policy.Instruments[j].Instrument.ID
	})
	sort.Slice(c.Marks, func(i, j int) bool { return c.Marks[i].Instrument.ID < c.Marks[j].Instrument.ID })
	payload, err := json.Marshal(struct {
		Context   SnapshotContext    `json:"context"`
		Portfolio contract.Portfolio `json:"portfolio"`
	}{c, portfolio})
	if err != nil {
		return "", fmt.Errorf("encode paper research snapshot: %w", err)
	}
	digest := sha256.Sum256(append([]byte("tradingbot.paperapply.snapshot.v1/"), payload...))
	digest[6] = digest[6]&0x0f | 0x80
	digest[8] = digest[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", digest[:4], digest[4:6], digest[6:8], digest[8:10], digest[10:16]), nil
}

func rawAmount(raw string, decimals int) (*big.Rat, error) {
	if !rawPattern.MatchString(raw) || decimals < 0 || decimals > 18 {
		return nil, fmt.Errorf("%w: invalid raw paper quantity", ErrInvalid)
	}
	units, _ := new(big.Int).SetString(raw, 10)
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	return new(big.Rat).SetFrac(units, scale), nil
}
