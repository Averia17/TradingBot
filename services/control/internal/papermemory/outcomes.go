// Package papermemory projects factual outcomes from the paper ledger event log.
package papermemory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"strings"
	"time"

	"tradingbot/control/internal/ledger"
)

const pageSize = 100

const sourceMode, accountingMethod = "paper_simulated_ledger", "fifo_recording_order_usd_scale8_v1"

// ErrNoHistory indicates that no account creation was available at the cutoff.
var ErrNoHistory = errors.New("no paper account history at the cutoff")

var (
	usdPattern      = regexp.MustCompile(`^(?:0|[1-9][0-9]{0,29})(?:\.[0-9]{1,8})?$`)
	quantityPattern = regexp.MustCompile(`^[1-9][0-9]{0,37}$`)
)

// EventReader reads the authoritative ledger stream in account version order.
type EventReader interface {
	Events(ctx context.Context, accountID string, afterVersion int64, limit int) ([]ledger.Event, error)
}

// Snapshot is a read-only view of observed paper accounting. Open lots carry
// cost basis only. The ledger has no marks, benchmark or external operating costs.
type Snapshot struct {
	AccountID         string    `json:"account_id"`
	AccountVersion    int64     `json:"account_version"`
	AsOf              time.Time `json:"as_of"`
	SourceMode        string    `json:"source_mode"`
	AccountingMethod  string    `json:"accounting_method"`
	CashUSD           string    `json:"cash_usd"`
	RecordedFeesUSD   string    `json:"recorded_fees_usd"`
	RealizedNetPnLUSD string    `json:"realized_net_pnl_usd"`
	Realized          []Outcome `json:"realized"`
	OpenLots          []OpenLot `json:"open_lots"`
}

// Outcome is identified by the account and Sell FillEventID. Timing and
// matched buy sources come from the original executions and their receipts.
type Outcome struct {
	FillEventID    string            `json:"fill_event_id"`
	OrderID        string            `json:"-"`
	DecisionID     string            `json:"decision_id"`
	Instrument     ledger.Instrument `json:"instrument"`
	QuantityRaw    string            `json:"quantity_raw"`
	GrossUSD       string            `json:"gross_usd"`
	FeeUSD         string            `json:"fee_usd"`
	CostBasisUSD   string            `json:"cost_basis_usd"`
	NetPnLUSD      string            `json:"net_pnl_usd"`
	ObservedAt     time.Time         `json:"observed_at"`
	AvailableAt    time.Time         `json:"available_at"`
	Lots           []LotAllocation   `json:"lots"`
	accountVersion int64
}

type LotAllocation struct {
	BuyFillEventID string `json:"buy_fill_event_id"`
	BuyDecisionID  string `json:"buy_decision_id"`
	QuantityRaw    string `json:"quantity_raw"`
	CostBasisUSD   string `json:"cost_basis_usd"`
}

type OpenLot struct {
	LotAllocation
	Instrument  ledger.Instrument `json:"instrument"`
	ObservedAt  time.Time         `json:"observed_at"`
	AvailableAt time.Time         `json:"available_at"`
}

// Load replays the contiguous prefix recorded by asOf in exact scale-8 USD
// units. The availability timestamp comes from the ledger's recording clock.
// Partial FIFO basis rounds down cumulatively per lot. The unallocated
// remainder stays with the open lot until it is fully disposed.
func Load(ctx context.Context, source EventReader, accountID string, asOf time.Time) (Snapshot, error) {
	var view Snapshot
	err := walkSnapshots(ctx, source, accountID, []time.Time{asOf}, nil, func(p *projection, cutoff time.Time) error {
		view = p.result(cutoff)
		return nil
	})
	return view, err
}

func walkSnapshots(ctx context.Context, source EventReader, accountID string, cutoffs []time.Time, applied func(ledger.Event) error, visit func(*projection, time.Time) error) error {
	if source == nil || accountID == "" || len(cutoffs) == 0 {
		return errors.New("paper projection requires a ledger source, account and cutoff")
	}
	for i, cutoff := range cutoffs {
		if cutoff.IsZero() || (i > 0 && !cutoff.After(cutoffs[i-1])) {
			return errors.New("paper projection cutoffs must be nonzero and increasing")
		}
	}
	p := projection{
		view: Snapshot{
			AccountID: accountID, SourceMode: sourceMode, AccountingMethod: accountingMethod,
			Realized: []Outcome{}, OpenLots: []OpenLot{},
		},
		orders: make(map[string]unsettledOrder),
		lots:   make(map[ledger.Instrument][]*lot),
	}
	var pending []ledger.Event
	exhausted := false
	for _, cutoff := range cutoffs {
		for !exhausted {
			if err := ctx.Err(); err != nil {
				return err
			}
			if len(pending) == 0 {
				var err error
				pending, err = source.Events(ctx, accountID, p.view.AccountVersion, pageSize)
				if err != nil {
					return fmt.Errorf("read paper ledger events: %w", err)
				}
				if len(pending) == 0 {
					exhausted = true
					break
				}
			}
			event := pending[0]
			if event.AccountVersion != p.view.AccountVersion+1 || event.RecordedAt.IsZero() || event.ID == "" {
				return fmt.Errorf("inconsistent paper event at account version %d", event.AccountVersion)
			}
			// Never skip an unavailable version and apply its later dependants,
			// even if a subsequent recorded timestamp is earlier after clock drift.
			if event.RecordedAt.After(cutoff) {
				break
			}
			if err := p.apply(event); err != nil {
				return fmt.Errorf("project paper event at account version %d: %w", event.AccountVersion, err)
			}
			if applied != nil {
				if err := applied(event); err != nil {
					return err
				}
			}
			p.view.AccountVersion = event.AccountVersion
			pending = pending[1:]
		}
		if p.view.AccountVersion == 0 {
			return ErrNoHistory
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := visit(&p, cutoff.UTC()); err != nil {
			return err
		}
	}
	return nil
}

type projection struct {
	view        Snapshot
	funding     Funding
	cash        big.Int
	fees        big.Int
	realized    big.Int
	availableAt time.Time
	orders      map[string]unsettledOrder
	lots        map[ledger.Instrument][]*lot
}

type unsettledOrder struct {
	request   ledger.ReservationRequest
	remaining *big.Int
}

type lot struct {
	open      OpenLot
	quantity  *big.Int
	basis     *big.Int
	consumed  big.Int
	allocated big.Int
}

func (p *projection) apply(e ledger.Event) error {
	if p.view.AccountVersion == 0 && e.Kind != "paper_account.created" {
		return ErrNoHistory
	}
	if e.RecordedAt.After(p.availableAt) {
		p.availableAt = e.RecordedAt.UTC()
	}
	switch e.Kind {
	case "paper_account.created":
		var request ledger.CreateAccount
		if e.AccountVersion != 1 || e.ID != p.view.AccountID || json.Unmarshal(e.Payload, &request) != nil || request.ID != p.view.AccountID {
			return errors.New("invalid account creation")
		}
		cash, err := usdUnits(request.InitialCashUSD)
		if err != nil {
			return err
		}
		p.cash.Set(cash)
		p.funding = Funding{CashUSD: formatUSD(cash), RecordedAt: e.RecordedAt.UTC()}
	case "paper_order.reserved":
		var request ledger.ReservationRequest
		if json.Unmarshal(e.Payload, &request) != nil || request.AccountID != p.view.AccountID || request.OrderID != e.ID ||
			request.DecisionID == "" || request.SnapshotVersion != p.view.AccountVersion ||
			(request.Side != "BUY" && request.Side != "SELL") || !quantityPattern.MatchString(request.QuantityRaw) ||
			request.Instrument.ID == "" || request.Instrument.RegistryVersion == "" || request.Instrument.Decimals < 0 || request.Instrument.Decimals > 18 {
			return errors.New("invalid paper reservation")
		}
		if _, exists := p.orders[request.OrderID]; exists {
			return errors.New("duplicate paper order")
		}
		quantity, _ := new(big.Int).SetString(request.QuantityRaw, 10)
		p.orders[request.OrderID] = unsettledOrder{request: request, remaining: quantity}
	case "paper_order.partially_filled", "paper_order.filled", "paper_order.unknown", "paper_order.cancelled":
		var resolution ledger.ResolutionRequest
		if json.Unmarshal(e.Payload, &resolution) != nil || resolution.AccountID != p.view.AccountID || resolution.EventID != e.ID || e.Kind != "paper_order."+resolution.Outcome {
			return errors.New("invalid paper resolution")
		}
		order, exists := p.orders[resolution.OrderID]
		if !exists {
			return errors.New("paper resolution lacks its reservation")
		}
		if resolution.Outcome == "cancelled" || resolution.Outcome == "filled" {
			delete(p.orders, resolution.OrderID)
		}
		if finished := resolution.Outcome == "filled"; finished || resolution.Outcome == "partially_filled" {
			if resolution.ObservedAt.IsZero() || resolution.ObservedAt.After(e.RecordedAt) {
				return errors.New("fill observation postdates its receipt")
			}
			quantity := new(big.Int).Set(order.remaining)
			if !finished || resolution.QuantityRaw != "" {
				if !quantityPattern.MatchString(resolution.QuantityRaw) {
					return errors.New("invalid fill quantity")
				}
				quantity.SetString(resolution.QuantityRaw, 10)
			}
			comparison := quantity.Cmp(order.remaining)
			if finished && comparison != 0 || !finished && comparison >= 0 {
				return errors.New("fill quantity conflicts with the unexecuted remainder")
			}
			order.remaining.Sub(order.remaining, quantity)
			return p.fill(order.request, resolution, quantity)
		}
		if resolution.QuantityRaw != "" || resolution.GrossUSD != "" || resolution.FeeUSD != "" || !resolution.ObservedAt.IsZero() {
			return errors.New("non-fill resolution contains execution amounts")
		}
	default:
		return fmt.Errorf("unsupported paper event kind %q", e.Kind)
	}
	return nil
}

func (p *projection) fill(order ledger.ReservationRequest, fill ledger.ResolutionRequest, quantity *big.Int) error {
	gross, err := usdUnits(fill.GrossUSD)
	if err != nil || gross.Sign() == 0 {
		return errors.New("invalid fill gross amount")
	}
	fee, err := usdUnits(fill.FeeUSD)
	if err != nil {
		return err
	}
	p.fees.Add(&p.fees, fee)
	if order.Side == "BUY" {
		basis := new(big.Int).Add(gross, fee)
		p.cash.Sub(&p.cash, basis)
		if p.cash.Sign() < 0 {
			return errors.New("paper buy exceeds cash")
		}
		p.lots[order.Instrument] = append(p.lots[order.Instrument], &lot{
			quantity: quantity, basis: basis,
			open: OpenLot{
				LotAllocation: LotAllocation{BuyFillEventID: fill.EventID, BuyDecisionID: order.DecisionID},
				Instrument:    order.Instrument, ObservedAt: fill.ObservedAt.UTC(), AvailableAt: p.availableAt,
			},
		})
		return nil
	}
	return p.sell(order, fill, quantity, gross, fee)
}

func (p *projection) sell(order ledger.ReservationRequest, fill ledger.ResolutionRequest, quantity, gross, fee *big.Int) error {
	remaining := new(big.Int).Set(quantity)
	lots := p.lots[order.Instrument]
	matches := make([]LotAllocation, 0, len(lots))
	var basis big.Int
	for remaining.Sign() > 0 && len(lots) != 0 {
		current := lots[0]
		take := new(big.Int).Sub(current.quantity, &current.consumed)
		if take.Cmp(remaining) > 0 {
			take.Set(remaining)
		}
		current.consumed.Add(&current.consumed, take)
		allocated := new(big.Int).Mul(current.basis, &current.consumed)
		allocated.Quo(allocated, current.quantity)
		increment := new(big.Int).Sub(allocated, &current.allocated)
		current.allocated.Set(allocated)
		basis.Add(&basis, increment)
		remaining.Sub(remaining, take)
		match := current.open.LotAllocation
		match.QuantityRaw, match.CostBasisUSD = take.String(), formatUSD(increment)
		matches = append(matches, match)
		if current.consumed.Cmp(current.quantity) == 0 {
			lots[0] = nil
			lots = lots[1:]
		}
	}
	if remaining.Sign() != 0 || gross.Cmp(fee) < 0 {
		return errors.New("paper sell exceeds matched lots or fee exceeds proceeds")
	}
	p.lots[order.Instrument] = lots
	proceeds := new(big.Int).Sub(gross, fee)
	p.cash.Add(&p.cash, proceeds)
	net := new(big.Int).Sub(proceeds, &basis)
	p.realized.Add(&p.realized, net)
	p.view.Realized = append(p.view.Realized, Outcome{
		FillEventID: fill.EventID, OrderID: order.OrderID, DecisionID: order.DecisionID, Instrument: order.Instrument,
		QuantityRaw: quantity.String(), GrossUSD: formatUSD(gross), FeeUSD: formatUSD(fee),
		CostBasisUSD: formatUSD(&basis), NetPnLUSD: formatUSD(net),
		ObservedAt: fill.ObservedAt.UTC(), AvailableAt: p.availableAt, Lots: matches,
		accountVersion: p.view.AccountVersion + 1,
	})
	return nil
}

func (p *projection) result(cutoff time.Time) Snapshot {
	p.view.AsOf = cutoff
	p.view.CashUSD = formatUSD(&p.cash)
	p.view.RecordedFeesUSD = formatUSD(&p.fees)
	p.view.RealizedNetPnLUSD = formatUSD(&p.realized)
	for _, lots := range p.lots {
		for _, l := range lots {
			open := l.open
			open.QuantityRaw = new(big.Int).Sub(l.quantity, &l.consumed).String()
			open.CostBasisUSD = formatUSD(new(big.Int).Sub(l.basis, &l.allocated))
			p.view.OpenLots = append(p.view.OpenLots, open)
		}
	}
	slices.SortFunc(p.view.OpenLots, func(a, b OpenLot) int {
		if cmp := strings.Compare(a.Instrument.ID, b.Instrument.ID); cmp != 0 {
			return cmp
		}
		if cmp := a.AvailableAt.Compare(b.AvailableAt); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.BuyFillEventID, b.BuyFillEventID)
	})
	return p.view
}

func usdUnits(value string) (*big.Int, error) {
	if !usdPattern.MatchString(value) {
		return nil, errors.New("invalid paper USD decimal")
	}
	whole, fractional, _ := strings.Cut(value, ".")
	units, _ := new(big.Int).SetString(whole+fractional+strings.Repeat("0", 8-len(fractional)), 10)
	return units, nil
}

func formatUSD(units *big.Int) string {
	negative := units.Sign() < 0
	digits := strings.TrimPrefix(units.String(), "-")
	if len(digits) <= 8 {
		digits = strings.Repeat("0", 9-len(digits)) + digits
	}
	i := len(digits) - 8
	whole, fraction := digits[:i], strings.TrimRight(digits[i:], "0")
	if fraction != "" {
		whole += "." + fraction
	}
	if negative {
		whole = "-" + whole
	}
	return whole
}
