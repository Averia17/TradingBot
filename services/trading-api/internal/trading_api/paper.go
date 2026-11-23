package trading_api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"tradingbot/trading-api/internal/accounts"
	"tradingbot/trading-api/internal/contract"
	"tradingbot/trading-api/internal/paper_memory"
)

// PaperAccountStore provides account operations through the owner or an HTTP client.
type PaperAccountStore interface {
	Ping(context.Context) error
	CreateAccount(context.Context, accounts.CreateAccount) (accounts.Account, bool, error)
	GetAccount(context.Context, string) (accounts.Account, error)
}

type paperOrderStore interface {
	Reserve(context.Context, accounts.ReservationRequest) (accounts.Order, bool, error)
	Resolve(context.Context, accounts.ResolutionRequest) (accounts.Order, bool, error)
	GetOrder(context.Context, string, string) (accounts.Order, error)
	Events(context.Context, string, int64, int) ([]accounts.Event, error)
}

type paperHTTP struct {
	store PaperAccountStore
}

type paperOrderHTTP struct {
	store paperOrderStore
	now   func() time.Time
}

func (h paperHTTP) create(w http.ResponseWriter, r *http.Request) {
	body, ok := readJSONBody(w, r)
	if !ok {
		return
	}
	var input accounts.CreateAccount
	if contract.Decode(body, contract.PaperAccountCreate, &input) != nil {
		writeError(w, http.StatusBadRequest, "invalid_paper_request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), jobStoreTimeout)
	defer cancel()
	account, created, err := h.store.CreateAccount(ctx, input)
	if err != nil {
		writePaperError(w, err)
		return
	}
	w.Header().Set("Location", "/v1/paper/accounts/"+account.ID)
	writeJSON(w, createStatus(created), account)
}

func (h paperHTTP) get(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), jobStoreTimeout)
	defer cancel()
	account, err := h.store.GetAccount(ctx, r.PathValue("id"))
	if err != nil {
		writePaperError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, account)
}

func (h paperOrderHTTP) reserve(w http.ResponseWriter, r *http.Request) {
	body, ok := readJSONBody(w, r)
	if !ok {
		return
	}
	var input accounts.ReservationRequest
	if contract.Decode(body, contract.PaperReserve, &input) != nil || input.AccountID != r.PathValue("id") {
		writeError(w, http.StatusBadRequest, "invalid_paper_request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), jobStoreTimeout)
	defer cancel()
	order, created, err := h.store.Reserve(ctx, input)
	if err != nil {
		writePaperError(w, err)
		return
	}
	w.Header().Set("Location", "/v1/paper/accounts/"+order.AccountID+"/orders/"+order.OrderID)
	writeJSON(w, createStatus(created), order)
}

func (h paperOrderHTTP) get(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), jobStoreTimeout)
	defer cancel()
	order, err := h.store.GetOrder(ctx, r.PathValue("id"), r.PathValue("order"))
	if err != nil {
		writePaperError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, order)
}

func (h paperOrderHTTP) resolve(w http.ResponseWriter, r *http.Request) {
	body, ok := readJSONBody(w, r)
	if !ok {
		return
	}
	var input accounts.ResolutionRequest
	if contract.Decode(body, contract.PaperResolve, &input) != nil || input.AccountID != r.PathValue("id") || input.OrderID != r.PathValue("order") {
		writeError(w, http.StatusBadRequest, "invalid_paper_request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), jobStoreTimeout)
	defer cancel()
	order, _, err := h.store.Resolve(ctx, input)
	if err != nil {
		writePaperError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, order)
}

func (h paperOrderHTTP) events(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	after, err := strconv.ParseInt(query.Get("after_version"), 10, 64)
	if !query.Has("after_version") {
		after, err = 0, nil
	}
	limit, limitErr := strconv.Atoi(query.Get("limit"))
	if !query.Has("limit") {
		limit, limitErr = 25, nil
	}
	if err != nil || limitErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_paper_request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), jobStoreTimeout)
	defer cancel()
	events, err := h.store.Events(ctx, r.PathValue("id"), after, limit)
	if err != nil {
		writePaperError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func (h paperOrderHTTP) outcomes(w http.ResponseWriter, r *http.Request) {
	asOf, err := time.Parse(time.RFC3339Nano, r.URL.Query().Get("as_of"))
	if err != nil || asOf.IsZero() || asOf.After(h.now()) {
		writeError(w, http.StatusBadRequest, "invalid_paper_cutoff")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), jobStoreTimeout)
	defer cancel()
	snapshot, err := paper_memory.Load(ctx, h.store, r.PathValue("id"), asOf)
	if err != nil {
		writePaperError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func writePaperError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, accounts.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_paper_request")
	case errors.Is(err, accounts.ErrNotFound), errors.Is(err, paper_memory.ErrNoHistory):
		writeError(w, http.StatusNotFound, "paper_record_not_found")
	case errors.Is(err, accounts.ErrStale):
		writeError(w, http.StatusConflict, "paper_snapshot_stale")
	case errors.Is(err, accounts.ErrExpired):
		writeError(w, http.StatusConflict, "paper_quote_expired")
	case errors.Is(err, accounts.ErrInsufficient):
		writeError(w, http.StatusConflict, "paper_insufficient_funds")
	case errors.Is(err, accounts.ErrConflict):
		writeError(w, http.StatusConflict, "paper_conflict")
	default:
		writeError(w, http.StatusServiceUnavailable, "paper_storage_unavailable")
	}
}
