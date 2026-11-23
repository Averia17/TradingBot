package main

import (
	"context"
	"errors"
	"os"

	"tradingbot/trading-api/internal/accounts"
	"tradingbot/trading-api/internal/accounts_http"
	"tradingbot/trading-api/internal/jobs"
	"tradingbot/trading-api/internal/jobs_http"
	"tradingbot/trading-api/internal/paper_thesis"
	"tradingbot/trading-api/internal/paper_workflow"
)

type researchService interface {
	paper_workflow.DispatchResearch
	paper_thesis.ScheduleStore
	ReadyPaperDispatch(context.Context, string, string) ([]jobs.PaperDispatchTask, error)
}

type accountService interface {
	paper_workflow.FollowStore
	paper_thesis.LedgerSource
	RiskActivity(context.Context, string, int64) (accounts.RiskActivity, error)
	Reconcile(context.Context, accounts.ReconciliationObservation, [32]byte) (accounts.Order, bool, error)
	Close()
}

func openPaperServices(ctx context.Context, fixtureDSN string) (researchService, accountService, func(), error) {
	if fixtureDSN != "" {
		research, paper, err := openFixtureStores(ctx, fixtureDSN)
		if err != nil {
			return nil, nil, nil, err
		}
		return research, paper, func() { paper.Close(); research.Close() }, nil
	}
	token, err := accounts_http.InternalToken()
	if err != nil {
		return nil, nil, nil, err
	}
	research, err := jobs_http.NewClient(os.Getenv("TRADINGBOT_TRADING_API_URL"), token)
	if err != nil {
		return nil, nil, nil, err
	}
	paper, err := accounts_http.NewClient(os.Getenv("TRADINGBOT_ACCOUNTS_URL"), token)
	if err != nil {
		research.Close()
		return nil, nil, nil, err
	}
	return research, paper, func() { paper.Close(); research.Close() }, nil
}

func openAccountService(ctx context.Context, fixtureDSN string) (accountService, error) {
	if fixtureDSN != "" {
		return accounts.Open(ctx, fixtureDSN)
	}
	token, err := accounts_http.InternalToken()
	if err != nil {
		return nil, err
	}
	return accounts_http.NewClient(os.Getenv("TRADINGBOT_ACCOUNTS_URL"), token)
}

// openFixtureStores keeps direct Postgres fixtures isolated from the service configuration.
func openFixtureStores(ctx context.Context, dsn string) (*jobs.Store, *accounts.Store, error) {
	ctx, cancel := context.WithTimeout(ctx, paperOperationTimeout)
	defer cancel()
	research, err := jobs.Open(ctx, dsn)
	if err != nil {
		return nil, nil, errors.New("research job storage startup failed; check PostgreSQL configuration")
	}
	paper, err := accounts.Open(ctx, dsn)
	if err != nil {
		research.Close()
		return nil, nil, errors.New("paper ledger startup failed; check PostgreSQL configuration")
	}
	return research, paper, nil
}
