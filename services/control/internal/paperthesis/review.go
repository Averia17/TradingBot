// Package paperthesis projects review deadlines from sourced paper positions.
package paperthesis

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"time"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/papermemory"
	"tradingbot/control/internal/researchscheduler"
)

const Version = "0.1"

var (
	ErrInvalid  = errors.New("invalid paper thesis history or review input")
	ErrUnlinked = errors.New("paper opening has no accepted research source")
	rawPattern  = regexp.MustCompile(`^(?:0|[1-9][0-9]{0,37})$`)
)

type ResearchSource interface {
	Get(context.Context, string) (jobs.Job, error)
	GetResult(context.Context, string) (jobs.Result, error)
}

type LedgerSource interface {
	papermemory.EventReader
	GetAccount(context.Context, string) (ledger.Account, error)
	GetOrder(context.Context, string, string) (ledger.Order, error)
}

type View struct {
	Version             string            `json:"paper_thesis_schema_version"`
	AccountID           string            `json:"account_id"`
	AccountVersion      int64             `json:"account_version"`
	AsOf                time.Time         `json:"as_of"`
	JobID               string            `json:"research_job_id"`
	DecisionID          string            `json:"decision_id"`
	OrderID             string            `json:"opening_order_id"`
	Instrument          ledger.Instrument `json:"instrument"`
	Symbol              string            `json:"symbol"`
	State               string            `json:"state"`
	OpeningOrderState   string            `json:"opening_order_state"`
	FirstFillEventID    string            `json:"first_fill_event_id,omitempty"`
	OpenedAt            *time.Time        `json:"opened_at,omitempty"`
	NextReviewAt        *time.Time        `json:"next_review_at,omitempty"`
	ExpiresAt           *time.Time        `json:"thesis_expires_at,omitempty"`
	Horizon             contract.Horizon  `json:"horizon"`
	SourceQuantityRaw   string            `json:"source_quantity_raw"`
	HeldQuantityRaw     string            `json:"held_quantity_raw"`
	ReservedQuantityRaw string            `json:"reserved_quantity_raw"`
	FreeQuantityRaw     string            `json:"free_quantity_raw"`
	CashUSD             string            `json:"cash_usd"`
	ReservedCashUSD     string            `json:"reserved_cash_usd"`
	ClockReview         *Trigger          `json:"clock_review,omitempty"`
}

type Trigger struct {
	ID            string    `json:"trigger_id"`
	Kind          string    `json:"kind"`
	Reason        string    `json:"reason"`
	At            time.Time `json:"at"`
	EventProvider string    `json:"event_provider,omitempty"`
	EventRecordID string    `json:"event_record_id,omitempty"`
	ArchiveSHA256 string    `json:"archive_sha256,omitempty"`
}

// Load reconstructs an opening thesis from the point-in-time ledger prefix.
// The current account must still match its terminal version before residual
// quantities and reservations can be used together. No transaction is created.
func Load(ctx context.Context, research ResearchSource, source LedgerSource, accountID, orderID string, asOf time.Time) (View, error) {
	view, err := projectOpening(ctx, research, source, accountID, orderID, asOf)
	if err != nil {
		return View{}, err
	}
	account, reserved, err := currentAccount(ctx, source, view)
	if err != nil {
		return View{}, err
	}
	held, _ := new(big.Int).SetString(view.HeldQuantityRaw, 10)
	view.ReservedQuantityRaw = reserved.String()
	view.FreeQuantityRaw = held.Sub(held, reserved).String()
	view.ReservedCashUSD = account.ReservedCashUSD
	return view, nil
}

// projectOpening derives the trigger from the historical prefix. This lets an
// accepted schedule replay against its original cutoff after the account moves.
func projectOpening(ctx context.Context, research ResearchSource, source LedgerSource, accountID, orderID string, asOf time.Time) (View, error) {
	if source == nil || research == nil || accountID == "" || orderID == "" || asOf.IsZero() {
		return View{}, ErrInvalid
	}
	order, err := source.GetOrder(ctx, accountID, orderID)
	if err != nil {
		return View{}, fmt.Errorf("read sourced paper opening: %w", err)
	}
	if order.ResearchJobID == "" {
		return View{}, ErrUnlinked
	}
	if order.Side != "BUY" || order.AccountID != accountID || order.OrderID != orderID {
		return View{}, ErrInvalid
	}
	trace := openingTrace{EventReader: source, orderID: orderID, asOf: asOf, fillIDs: make(map[string]struct{})}
	book, err := papermemory.Load(ctx, &trace, accountID, asOf)
	if err != nil {
		return View{}, err
	}
	if trace.reservedAt.IsZero() || trace.request != order.ReservationRequest {
		return View{}, fmt.Errorf("%w: opening reservation does not match its event", ErrInvalid)
	}
	decision, err := openingDecision(ctx, research, order.ReservationRequest, trace.reservedAt)
	if err != nil {
		return View{}, err
	}
	held, sourced, err := openQuantities(book.OpenLots, order.ReservationRequest, trace.fillIDs)
	if err != nil {
		return View{}, err
	}
	view := View{
		Version: Version, AccountID: accountID, AccountVersion: book.AccountVersion, AsOf: asOf.UTC(),
		JobID: order.ResearchJobID, DecisionID: order.DecisionID, OrderID: orderID,
		Instrument: order.Instrument, Symbol: decision.Symbol, OpeningOrderState: trace.state,
		FirstFillEventID: trace.firstFillID, Horizon: decision.Horizon,
		HeldQuantityRaw: held.String(), SourceQuantityRaw: sourced.String(), CashUSD: book.CashUSD,
	}
	if trace.openedAt.IsZero() {
		view.State = "pending"
		if trace.state == "cancelled" {
			view.State = "unopened"
		}
		return view, nil
	}
	expiry := trace.openedAt.Add(time.Duration(view.Horizon.Max) * time.Hour)
	view.OpenedAt, view.ExpiresAt = &trace.openedAt, &expiry
	if sourced.Sign() == 0 {
		view.State = "closed"
		return view, nil
	}
	view.State = "open"
	view.NextReviewAt, view.ClockReview = clock(view, trace.openedAt, expiry)
	return view, nil
}

type openingTrace struct {
	papermemory.EventReader
	orderID, state, firstFillID      string
	asOf, reservedAt, openedAt, seen time.Time
	request                          ledger.ReservationRequest
	fillIDs                          map[string]struct{}
}

// Capture the opening facts while the common paper ledger projection verifies
// and accounts for the same event pages in order.
func (t *openingTrace) Events(ctx context.Context, accountID string, after int64, limit int) ([]ledger.Event, error) {
	events, err := t.EventReader.Events(ctx, accountID, after, limit)
	if err != nil {
		return nil, err
	}
	for _, event := range events {
		if event.RecordedAt.After(t.asOf) {
			break
		}
		if event.RecordedAt.After(t.seen) {
			t.seen = event.RecordedAt.UTC()
		}
		if event.Kind == "paper_order.reserved" && event.ID == t.orderID {
			if err := json.Unmarshal(event.Payload, &t.request); err != nil {
				return nil, fmt.Errorf("%w: malformed opening ledger event", ErrInvalid)
			}
			t.reservedAt, t.state = t.seen, "reserved"
		}
		switch event.Kind {
		case "paper_order.filled", "paper_order.partially_filled", "paper_order.unknown", "paper_order.cancelled":
			var resolution ledger.ResolutionRequest
			if err := json.Unmarshal(event.Payload, &resolution); err != nil {
				return nil, fmt.Errorf("%w: malformed resolution ledger event", ErrInvalid)
			}
			if resolution.OrderID != t.orderID {
				continue
			}
			t.state = resolution.Outcome
			if t.state == "filled" || t.state == "partially_filled" {
				t.fillIDs[event.ID] = struct{}{}
				if t.openedAt.IsZero() {
					t.openedAt, t.firstFillID = t.seen, event.ID
				}
			}
		}
	}
	return events, nil
}

func openQuantities(lots []papermemory.OpenLot, order ledger.ReservationRequest, fillIDs map[string]struct{}) (held, sourced *big.Int, err error) {
	total, own := new(big.Int), new(big.Int)
	for _, lot := range lots {
		_, fromOpening := fillIDs[lot.BuyFillEventID]
		if fromOpening && (lot.Instrument != order.Instrument || lot.BuyDecisionID != order.DecisionID) {
			return nil, nil, ErrInvalid
		}
		if lot.Instrument != order.Instrument {
			continue
		}
		units, _ := new(big.Int).SetString(lot.QuantityRaw, 10) // Validated by papermemory.
		total.Add(total, units)
		if fromOpening {
			own.Add(own, units)
		}
	}
	return total, own, nil
}

func currentAccount(ctx context.Context, source LedgerSource, view View) (ledger.Account, *big.Int, error) {
	account, err := source.GetAccount(ctx, view.AccountID)
	if err != nil {
		return ledger.Account{}, nil, fmt.Errorf("read current paper account: %w", err)
	}
	if account.ID != view.AccountID || account.Version != view.AccountVersion {
		return ledger.Account{}, nil, ledger.ErrStale
	}
	if account.CashUSD != view.CashUSD {
		return ledger.Account{}, nil, fmt.Errorf("%w: versioned account cash differs from history", ErrInvalid)
	}
	held, reserved := new(big.Int), new(big.Int)
	seen := false
	for _, pos := range account.Positions {
		if pos.Instrument != view.Instrument {
			continue
		}
		if seen || !rawPattern.MatchString(pos.QuantityRaw) || !rawPattern.MatchString(pos.ReservedQuantityRaw) {
			return ledger.Account{}, nil, ErrInvalid
		}
		seen = true
		held.SetString(pos.QuantityRaw, 10)
		reserved.SetString(pos.ReservedQuantityRaw, 10)
	}
	if held.String() != view.HeldQuantityRaw || held.Cmp(reserved) < 0 {
		return ledger.Account{}, nil, fmt.Errorf("%w: position balance differs from ledger history", ErrInvalid)
	}
	return account, reserved, nil
}

func clock(view View, opened, expiry time.Time) (*time.Time, *Trigger) {
	if !view.AsOf.Before(expiry) {
		return nil, clockTrigger(view, "expiry", expiry)
	}
	interval := time.Duration(view.Horizon.Min) * time.Hour
	if view.AsOf.Before(opened.Add(interval)) {
		next := opened.Add(interval)
		return &next, nil
	}
	window := view.AsOf.Sub(opened) / interval
	current := opened.Add(window * interval)
	next := current.Add(interval)
	if expiry.Before(next) {
		next = expiry
	}
	return &next, clockTrigger(view, "cadence", current)
}

func clockTrigger(view View, reason string, at time.Time) *Trigger {
	return &Trigger{ID: triggerID(view, reason+"\n"+at.Format(time.RFC3339Nano)), Kind: "review", Reason: reason, At: at}
}

// CatalystReview recognizes independently hash-verified archive evidence for an
// open thesis. Replaying the same provider record keeps the same trigger ID.
func CatalystReview(view View, event researchscheduler.VerifiedEvent) (Trigger, error) {
	at := event.KnownAt()
	if view.State != "open" || view.OpenedAt == nil || at.IsZero() || at.Before(*view.OpenedAt) || at.After(view.AsOf) ||
		event.Cutoff().After(view.AsOf) || (event.Symbol() != "" && event.Symbol() != view.Symbol) {
		return Trigger{}, fmt.Errorf("%w: catalyst was not available for the open thesis", ErrInvalid)
	}
	return Trigger{
		ID: triggerID(view, "catalyst\n"+event.Provider()+"\n"+event.RecordID()), Kind: event.Kind(), Reason: "catalyst", At: at,
		EventProvider: event.Provider(), EventRecordID: event.RecordID(), ArchiveSHA256: event.ArchiveSHA256(),
	}, nil
}

func triggerID(view View, key string) string {
	h := sha256.Sum256([]byte("tradingbot/paper-thesis-review-v1\n" + view.AccountID + "\n" + view.OrderID + "\n" + key))
	h[6] = h[6]&0x0f | 0x80
	h[8] = h[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[:4], h[4:6], h[6:8], h[8:10], h[10:16])
}
