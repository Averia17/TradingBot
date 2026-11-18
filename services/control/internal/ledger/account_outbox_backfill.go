package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// backfillAccountOutbox runs inside the migration, while ledger writers are
// blocked. The final comparison detects incomplete or inconsistent old history
// before any account events become visible to the relay.
func backfillAccountOutbox(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, `SELECT account_id::text FROM paper_accounts ORDER BY account_id`)
	if err != nil {
		return err
	}
	accounts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, accountID := range accounts {
		if err := backfillAccountHistory(ctx, tx, accountID); err != nil {
			return fmt.Errorf("backfill paper analytics for account %s: %w", accountID, err)
		}
	}
	return nil
}

func backfillAccountHistory(ctx context.Context, tx pgx.Tx, accountID string) error {
	history := accountHistory{
		orders:    make(map[string]*historicOrder),
		positions: make(map[string]Position),
	}
	var version int64
	for {
		events, err := readEvents(ctx, tx, accountID, version, 100)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			return history.reconcile(ctx, tx, accountID, version)
		}
		for _, event := range events {
			if event.AccountVersion != version+1 {
				return fmt.Errorf("paper ledger account version gap after %d", version)
			}
			if err := history.append(ctx, tx, accountID, event); err != nil {
				return fmt.Errorf("paper ledger version %d: %w", event.AccountVersion, err)
			}
			version = event.AccountVersion
		}
	}
}

type accountHistory struct {
	cash, reservedCash big.Rat
	orders             map[string]*historicOrder
	positions          map[string]Position
}

type historicOrder struct {
	request   ReservationRequest
	remaining *big.Int
	feeBudget *big.Rat
	unknown   bool
}

func (h *accountHistory) append(ctx context.Context, tx pgx.Tx, accountID string, e Event) error {
	if e.RecordedAt.IsZero() || (e.AccountVersion == 1) != (e.Kind == "paper_account.created") {
		return errors.New("invalid event metadata")
	}
	wire := accountAnalyticsEvent{
		Version: "1.0", AccountID: accountID, EventID: e.ID, AccountVersion: e.AccountVersion, Kind: e.Kind, RecordedAt: e.RecordedAt.UTC(),
	}
	switch e.Kind {
	case "paper_account.created":
		var funded CreateAccount
		if json.Unmarshal(e.Payload, &funded) != nil || funded.ID != accountID || e.ID != accountID || !moneyPattern.MatchString(funded.InitialCashUSD) {
			return errors.New("invalid initial funding event")
		}
		h.cash.Set(amount(funded.InitialCashUSD))
	case "paper_order.reserved":
		var request ReservationRequest
		if json.Unmarshal(e.Payload, &request) != nil || request.AccountID != accountID || request.OrderID != e.ID ||
			request.SnapshotVersion != e.AccountVersion-1 || !request.valid() {
			return errors.New("invalid reservation event")
		}
		position, err := h.reserve(request)
		if err != nil {
			return err
		}
		wire.Position = &position
	case "paper_order.partially_filled", "paper_order.filled", "paper_order.cancelled", "paper_order.unknown":
		var resolution ResolutionRequest
		if json.Unmarshal(e.Payload, &resolution) != nil || resolution.AccountID != accountID || resolution.EventID != e.ID ||
			"paper_order."+resolution.Outcome != e.Kind || !resolution.valid() {
			return errors.New("invalid resolution event")
		}
		position, fill, err := h.resolve(ctx, tx, e, resolution)
		if err != nil {
			return err
		}
		wire.Position, wire.Fill = &position, fill
	default:
		return fmt.Errorf("unsupported paper ledger kind %q", e.Kind)
	}
	if h.cash.Sign() < 0 || h.reservedCash.Sign() < 0 || h.reservedCash.Cmp(&h.cash) > 0 {
		return errors.New("inconsistent settled or reserved cash")
	}
	wire.CashUSD, wire.ReservedCashUSD = analyticsCash(&h.cash), analyticsCash(&h.reservedCash)
	return writeAccountOutbox(ctx, tx, wire)
}

func (h *accountHistory) reserve(request ReservationRequest) (Position, error) {
	position, exists := h.positions[request.Instrument.ID]
	if !exists {
		if request.Side != "BUY" {
			return Position{}, errors.New("sell reservation lacks a held instrument")
		}
		position = Position{Instrument: request.Instrument, QuantityRaw: "0", ReservedQuantityRaw: "0"}
	}
	if position.Instrument != request.Instrument || h.orders[request.OrderID] != nil {
		return Position{}, errors.New("conflicting reservation or held instrument")
	}
	order := &historicOrder{request: request, remaining: rawUnits(request.QuantityRaw), feeBudget: amount(request.MaxFeeUSD)}
	if request.Side == "BUY" {
		h.reservedCash.Add(&h.reservedCash, buyReservation(Order{ReservationRequest: request}, order.remaining, order.feeBudget))
	} else {
		reserved := new(big.Int).Add(rawUnits(position.ReservedQuantityRaw), order.remaining)
		if reserved.Cmp(rawUnits(position.QuantityRaw)) > 0 {
			return Position{}, errors.New("sell reservation exceeds held quantity")
		}
		position.ReservedQuantityRaw = reserved.String()
	}
	h.orders[request.OrderID] = order
	h.positions[position.ID] = position
	return position, nil
}

func (h *accountHistory) resolve(ctx context.Context, tx pgx.Tx, event Event, r ResolutionRequest) (Position, *accountAnalyticsFill, error) {
	order := h.orders[r.OrderID]
	if order == nil {
		return Position{}, nil, errors.New("resolution lacks an open reservation")
	}
	position := h.positions[order.request.Instrument.ID]
	if r.Outcome == "unknown" {
		if order.unknown {
			return Position{}, nil, errors.New("repeated unknown resolution")
		}
		order.unknown = true
		return position, nil, nil
	}
	quantity, gross, fee := new(big.Int), new(big.Rat), new(big.Rat)
	var wireFill *accountAnalyticsFill
	if r.isFill() {
		fill, err := readHistoricFill(ctx, tx, order.request, event, r)
		if err != nil {
			return Position{}, nil, err
		}
		wireFill = &fill
		quantity, gross, fee = rawUnits(fill.QuantityRaw), amount(fill.GrossUSD), amount(fill.FeeUSD)
		comparison := quantity.Cmp(order.remaining)
		if (r.Outcome == "filled" && comparison != 0) || (r.Outcome == "partially_filled" && comparison >= 0) || fee.Cmp(order.feeBudget) > 0 {
			return Position{}, nil, errors.New("fill does not match remaining reserve")
		}
	}
	if order.request.Side == "BUY" {
		authorized := Order{ReservationRequest: order.request}
		released := buyReservation(authorized, order.remaining, order.feeBudget)
		if r.Outcome == "partially_filled" {
			order.remaining.Sub(order.remaining, quantity)
			order.feeBudget.Sub(order.feeBudget, fee)
			released.Sub(released, buyReservation(authorized, order.remaining, order.feeBudget))
		}
		h.reservedCash.Sub(&h.reservedCash, released)
		h.cash.Sub(&h.cash, gross).Sub(&h.cash, fee)
		position.QuantityRaw = new(big.Int).Add(rawUnits(position.QuantityRaw), quantity).String()
	} else {
		released := order.remaining
		if r.Outcome == "partially_filled" {
			released = quantity
			order.remaining.Sub(order.remaining, quantity)
			order.feeBudget.Sub(order.feeBudget, fee)
		}
		remaining := new(big.Int).Sub(rawUnits(position.QuantityRaw), quantity)
		reserved := new(big.Int).Sub(rawUnits(position.ReservedQuantityRaw), released)
		if reserved.Sign() < 0 || remaining.Cmp(reserved) < 0 || gross.Cmp(fee) < 0 {
			return Position{}, nil, errors.New("inconsistent sell reservation or proceeds")
		}
		position.QuantityRaw, position.ReservedQuantityRaw = remaining.String(), reserved.String()
		h.cash.Add(&h.cash, gross).Sub(&h.cash, fee)
	}
	if r.Outcome == "partially_filled" {
		order.unknown = false
	} else {
		delete(h.orders, r.OrderID)
	}
	h.positions[position.ID] = position
	return position, wireFill, nil
}

func readHistoricFill(ctx context.Context, tx pgx.Tx, order ReservationRequest, event Event, r ResolutionRequest) (accountAnalyticsFill, error) {
	fill := accountAnalyticsFill{Side: order.Side, OrderID: order.OrderID, DecisionID: order.DecisionID,
		QuoteID: order.QuoteID, ResearchJobID: order.ResearchJobID}
	err := tx.QueryRow(ctx, `SELECT quantity_raw::text, trim_scale(gross_usd)::text, trim_scale(fee_usd)::text, observed_at
		FROM paper_fills WHERE account_id=$1 AND order_id=$2 AND event_id=$3`, order.AccountID, order.OrderID, event.ID).
		Scan(&fill.QuantityRaw, &fill.GrossUSD, &fill.FeeUSD, &fill.ObservedAt)
	if err != nil {
		return accountAnalyticsFill{}, fmt.Errorf("read booked fill: %w", err)
	}
	fill.ObservedAt = fill.ObservedAt.UTC()
	if !rawPattern.MatchString(fill.QuantityRaw) || (r.QuantityRaw != "" && fill.QuantityRaw != r.QuantityRaw) ||
		!moneyPattern.MatchString(fill.GrossUSD) || !moneyPattern.MatchString(fill.FeeUSD) ||
		compareAmounts(fill.GrossUSD, r.GrossUSD) != 0 || compareAmounts(fill.FeeUSD, r.FeeUSD) != 0 ||
		!fill.ObservedAt.Equal(r.ObservedAt.UTC().Truncate(time.Microsecond)) || fill.ObservedAt.After(event.RecordedAt) {
		return accountAnalyticsFill{}, errors.New("booked fill disagrees with its ledger event")
	}
	return fill, nil
}

func (h *accountHistory) reconcile(ctx context.Context, tx pgx.Tx, accountID string, version int64) error {
	account, err := readAccount(ctx, tx, accountID)
	if err != nil {
		return err
	}
	if account.Version != version || analyticsCash(&h.cash) != account.CashUSD || analyticsCash(&h.reservedCash) != account.ReservedCashUSD {
		return errors.New("replayed account head or cash disagrees with the ledger")
	}
	rows, err := tx.Query(ctx, `SELECT instrument_id, registry_version, decimals, quantity_raw::text, reserved_quantity_raw::text
		FROM paper_positions WHERE account_id=$1`, accountID)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var position Position
		if err := rows.Scan(&position.ID, &position.RegistryVersion, &position.Decimals, &position.QuantityRaw, &position.ReservedQuantityRaw); err != nil {
			return err
		}
		if h.positions[position.ID] != position {
			return fmt.Errorf("replayed position %s disagrees with the ledger", position.ID)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count != len(h.positions) {
		return errors.New("replayed positions are missing from the ledger")
	}
	return nil
}

func analyticsCash(value *big.Rat) string {
	return strings.TrimRight(strings.TrimRight(value.FloatString(8), "0"), ".")
}
