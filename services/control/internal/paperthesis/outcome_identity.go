package paperthesis

import (
	"context"
	"fmt"
	"os"
	"time"

	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paperapply"
	"tradingbot/control/internal/paperquotes"
)

type ExitApplication struct {
	FillEventID       string `json:"fill_event_id"`
	OrderID           string `json:"order_id"`
	ApplicationSHA256 string `json:"application_sha256"`
}

func verifyIdentitySales(ctx context.Context, research ResearchSource, source openingSource,
	opening openingProjection, exitFillIDs []string) ([]ExitApplication, error) {
	if len(exitFillIDs) == 0 {
		return nil, nil
	}
	withDispatch, canReadDispatch := research.(interface {
		ResearchSource
		GetPaperDispatch(context.Context, string) (jobs.PaperDispatchTask, error)
	})
	withApplication, canReadApplication := source.(interface {
		GetApplication(context.Context, string) (ledger.ApplicationRecord, error)
	})
	if !canReadDispatch || !canReadApplication {
		return nil, ErrUnlinked
	}

	selected := make(map[string]bool, len(exitFillIDs))
	for _, id := range exitFillIDs {
		selected[id] = true
	}
	view := opening.view
	var exits []ExitApplication
	byOrder := make(map[string]paperapply.ExitIdentity)
	for _, sale := range opening.book.Realized {
		if !selected[sale.FillEventID] {
			continue
		}
		exit, verified := byOrder[sale.OrderID]
		if !verified {
			order, err := source.GetOrder(ctx, view.AccountID, sale.OrderID)
			if err != nil {
				return nil, fmt.Errorf("read identity outcome exit %s: %w", sale.OrderID, err)
			}
			if order.AccountID != view.AccountID || order.OrderID != sale.OrderID || order.DecisionID != sale.DecisionID || order.ResearchJobID == "" {
				return nil, ErrUnlinked
			}
			exit, err = paperapply.ReadExitIdentity(ctx, withDispatch, withApplication, order, sale.AvailableAt)
			if err != nil {
				return nil, fmt.Errorf("verify identity outcome exit %s: %w", sale.OrderID, err)
			}
			byOrder[sale.OrderID] = exit
		}
		if exit.SecurityID != view.OpeningIdentity.SecurityID || exit.Instrument != view.Instrument || sale.Instrument != view.Instrument {
			return nil, fmt.Errorf("%w: FIFO exit lacks the opening security identity", ErrUnlinked)
		}
		exits = append(exits, ExitApplication{sale.FillEventID, sale.OrderID, exit.ApplicationSHA256})
	}
	if len(exits) != len(exitFillIDs) {
		return nil, ErrUnlinked
	}
	return exits, nil
}

func identityHorizonExit(archiveRoot string, reference, registry *paperapply.ArchiveReference, view View,
	horizon *HorizonOutcome, quantity string, age time.Duration) (*ExitObservation, error) {
	if reference == nil {
		return nil, nil
	}
	root, err := os.OpenRoot(archiveRoot)
	if err != nil {
		return nil, fmt.Errorf("open identity outcome archive: %w", err)
	}
	defer root.Close()
	tape, err := paperquotes.OpenIdentityTape(root, reference.Ref, reference.SHA256, view.AccountID, registry.SHA256)
	if err != nil {
		return nil, fmt.Errorf("verify identity outcome exit tape: %w", err)
	}
	horizon.TapeSHA256, horizon.ExitRegistry = tape.SHA256(), registry
	mark, found, err := tape.HorizonExit(view.OpeningIdentity.SecurityID, view.Instrument, quantity, horizon.AsOf, age)
	if err != nil {
		return nil, err
	}
	cutoffs := []time.Time{horizon.AsOf}
	if found {
		cutoffs = append(cutoffs, mark.Quote.ObservedAt)
	}
	binding := paperapply.ResearchSecurityBinding{Symbol: view.Symbol, SecurityID: view.OpeningIdentity.SecurityID}
	eligible, err := paperapply.InstrumentEligibleAt(root, *registry, binding, view.Instrument, cutoffs...)
	if err != nil || !eligible || !found {
		return nil, err
	}
	quote := mark.Quote
	fee, err := quote.FeeUSD()
	if err != nil {
		return nil, err
	}
	horizon.QuoteSHA256 = mark.SHA256
	return &ExitObservation{
		ID: mark.SHA256, Instrument: quote.Instrument, QuantityRaw: quote.QuantityRaw,
		GrossUSD: quote.GrossUSD, FeeUSD: fee, ObservedAt: quote.ObservedAt,
		AvailableAt: mark.AvailableAt, ExpiresAt: quote.ExpiresAt,
	}, nil
}
