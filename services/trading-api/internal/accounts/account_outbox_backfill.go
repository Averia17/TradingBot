package accounts

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
		orders:    make(map[string]orderAccounting),
		positions: make(map[string]Position),
	}
	var version int64
	for {
		events, err := readBackfillPage(ctx, tx, accountID, version)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			return history.reconcile(ctx, tx, accountID, version)
		}
		var outbox pgx.Batch
		firstVersion := version + 1
		for _, event := range events {
			if event.AccountVersion != version+1 {
				return fmt.Errorf("paper ledger account version gap after %d", version)
			}
			projected, err := history.project(accountID, event)
			if err != nil {
				return fmt.Errorf("paper ledger version %d: %w", event.AccountVersion, err)
			}
			payload, err := encodeAccountOutboxEvent(projected)
			if err != nil {
				return err
			}
			outbox.Queue(insertAccountOutboxSQL, projected.AccountID, projected.AccountVersion,
				projected.EventID, payload, projected.RecordedAt)
			version = event.AccountVersion
		}
		if err := writeBackfillPage(ctx, tx, &outbox, firstVersion); err != nil {
			return err
		}
	}
}

type backfillEvent struct {
	Event
	bookedOrderID string
	bookedFill    Fill
}

func readBackfillPage(ctx context.Context, tx pgx.Tx, accountID string, afterVersion int64) ([]backfillEvent, error) {
	const pageSize = 100
	rows, err := tx.Query(ctx, `SELECT e.event_id::text, e.account_version, e.kind, e.payload, e.recorded_at,
		COALESCE(f.order_id::text,''), COALESCE(f.quantity_raw::text,''),
		COALESCE(trim_scale(f.gross_usd)::text,''), COALESCE(trim_scale(f.fee_usd)::text,''), f.observed_at
		FROM (SELECT account_id, event_id, account_version, kind, payload, recorded_at FROM paper_events
			WHERE account_id=$1 AND account_version>$2 ORDER BY account_version LIMIT $3) e
		LEFT JOIN paper_fills f USING (account_id,event_id) ORDER BY e.account_version`, accountID, afterVersion, pageSize)
	if err != nil {
		return nil, fmt.Errorf("query paper events for backfill: %w", err)
	}
	defer rows.Close()
	var page []backfillEvent
	for rows.Next() {
		var event backfillEvent
		var observedAt *time.Time
		fill := &event.bookedFill
		if err := rows.Scan(&event.ID, &event.AccountVersion, &event.Kind, &event.Payload, &event.RecordedAt,
			&event.bookedOrderID, &fill.QuantityRaw, &fill.GrossUSD, &fill.FeeUSD, &observedAt); err != nil {
			return nil, fmt.Errorf("read paper event for backfill: %w", err)
		}
		event.RecordedAt = event.RecordedAt.UTC()
		if observedAt != nil {
			fill.EventID, fill.ObservedAt = event.ID, observedAt.UTC()
		}
		page = append(page, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read paper events for backfill: %w", err)
	}
	return page, nil
}

func writeBackfillPage(ctx context.Context, tx pgx.Tx, outbox *pgx.Batch, firstVersion int64) error {
	results := tx.SendBatch(ctx, outbox)
	defer results.Close()
	for index := range outbox.Len() {
		if _, err := results.Exec(); err != nil {
			return fmt.Errorf("enqueue paper account analytics version %d: %w", firstVersion+int64(index), err)
		}
	}
	return results.Close()
}

type accountHistory struct {
	cash, reservedCash big.Rat
	orders             map[string]orderAccounting
	positions          map[string]Position
}

func (h *accountHistory) project(accountID string, e backfillEvent) (accountAnalyticsEvent, error) {
	if e.RecordedAt.IsZero() || (e.AccountVersion == 1) != (e.Kind == "paper_account.created") {
		return accountAnalyticsEvent{}, errors.New("invalid event metadata")
	}
	wire := accountAnalyticsEvent{
		Version: "1.0", AccountID: accountID, EventID: e.ID, AccountVersion: e.AccountVersion, Kind: e.Kind, RecordedAt: e.RecordedAt.UTC(),
	}
	switch e.Kind {
	case "paper_account.created":
		var funded CreateAccount
		if json.Unmarshal(e.Payload, &funded) != nil || funded.ID != accountID || e.ID != accountID || !moneyPattern.MatchString(funded.InitialCashUSD) {
			return accountAnalyticsEvent{}, errors.New("invalid initial funding event")
		}
		h.cash.Set(amount(funded.InitialCashUSD))
	case "paper_order.reserved":
		var request ReservationRequest
		if json.Unmarshal(e.Payload, &request) != nil || request.AccountID != accountID || request.OrderID != e.ID ||
			request.SnapshotVersion != e.AccountVersion-1 || !request.valid() {
			return accountAnalyticsEvent{}, errors.New("invalid reservation event")
		}
		position, err := h.reserve(request)
		if err != nil {
			return accountAnalyticsEvent{}, err
		}
		wire.Position = &position
	case "paper_order.partially_filled", "paper_order.filled", "paper_order.cancelled", "paper_order.unknown":
		var resolution ResolutionRequest
		if json.Unmarshal(e.Payload, &resolution) != nil || resolution.AccountID != accountID || resolution.EventID != e.ID ||
			"paper_order."+resolution.Outcome != e.Kind || !resolution.valid() {
			return accountAnalyticsEvent{}, errors.New("invalid resolution event")
		}
		order, exists := h.orders[resolution.OrderID]
		if !exists {
			return accountAnalyticsEvent{}, errors.New("resolution lacks an open reservation")
		}
		var fill *Fill
		if resolution.isFill() {
			booked, err := e.validateBookedFill(resolution)
			if err != nil {
				return accountAnalyticsEvent{}, err
			}
			fill = &booked
			analytics := accountFill(order.request, booked)
			wire.Fill = &analytics
		}
		position, err := h.resolve(order, resolution.Outcome, fill)
		if err != nil {
			return accountAnalyticsEvent{}, err
		}
		wire.Position = &position
	default:
		return accountAnalyticsEvent{}, fmt.Errorf("unsupported paper ledger kind %q", e.Kind)
	}
	wire.CashUSD, wire.ReservedCashUSD = analyticsCash(&h.cash), analyticsCash(&h.reservedCash)
	return wire, nil
}

func (h *accountHistory) reserve(request ReservationRequest) (Position, error) {
	position, exists := h.positions[request.Instrument.ID]
	if !exists {
		if request.Side != "BUY" {
			return Position{}, errors.New("sell reservation lacks a held instrument")
		}
		position = Position{Instrument: request.Instrument, QuantityRaw: "0", ReservedQuantityRaw: "0"}
	}
	if _, exists := h.orders[request.OrderID]; exists || position.Instrument != request.Instrument {
		return Position{}, errors.New("conflicting reservation or held instrument")
	}
	order, change := reserveOrder(request)
	position, err := h.apply(position, change)
	if err != nil {
		return Position{}, err
	}
	h.orders[request.OrderID] = order
	return position, nil
}

func (h *accountHistory) resolve(order orderAccounting, outcome string, fill *Fill) (Position, error) {
	next, change, err := order.resolve(outcome, fill)
	if err != nil {
		return Position{}, err
	}
	position, err := h.apply(h.positions[order.request.Instrument.ID], change)
	if err != nil {
		return Position{}, err
	}
	if next.closed() {
		delete(h.orders, order.request.OrderID)
	} else {
		h.orders[order.request.OrderID] = next
	}
	return position, nil
}

func (h *accountHistory) apply(position Position, change balanceChange) (Position, error) {
	quantity := new(big.Int).Add(rawUnits(position.QuantityRaw), change.quantity)
	reservedQuantity := new(big.Int).Add(rawUnits(position.ReservedQuantityRaw), change.reservedQuantity)
	if reservedQuantity.Sign() < 0 || quantity.Cmp(reservedQuantity) < 0 {
		return Position{}, errors.New("inconsistent settled or reserved quantity")
	}
	cash := new(big.Rat).Add(&h.cash, change.cash)
	reservedCash := new(big.Rat).Add(&h.reservedCash, change.reservedCash)
	if reservedCash.Sign() < 0 || cash.Cmp(reservedCash) < 0 {
		return Position{}, errors.New("inconsistent settled or reserved cash")
	}
	position.QuantityRaw, position.ReservedQuantityRaw = quantity.String(), reservedQuantity.String()
	h.cash.Set(cash)
	h.reservedCash.Set(reservedCash)
	h.positions[position.ID] = position
	return position, nil
}

func (e backfillEvent) validateBookedFill(r ResolutionRequest) (Fill, error) {
	fill := e.bookedFill
	if e.bookedOrderID != r.OrderID || fill.EventID != e.ID ||
		!rawPattern.MatchString(fill.QuantityRaw) || (r.QuantityRaw != "" && fill.QuantityRaw != r.QuantityRaw) ||
		!moneyPattern.MatchString(fill.GrossUSD) || !moneyPattern.MatchString(fill.FeeUSD) ||
		compareAmounts(fill.GrossUSD, r.GrossUSD) != 0 || compareAmounts(fill.FeeUSD, r.FeeUSD) != 0 ||
		!fill.ObservedAt.Equal(r.ObservedAt.UTC().Truncate(time.Microsecond)) || fill.ObservedAt.After(e.RecordedAt) {
		return Fill{}, errors.New("booked fill disagrees with its ledger event")
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
