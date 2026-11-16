package control

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/papermemory"
)

type paperAccountStore interface {
	Ping(context.Context) error
	CreateAccount(context.Context, ledger.CreateAccount) (ledger.Account, bool, error)
	GetAccount(context.Context, string) (ledger.Account, error)
}

type paperOrderStore interface {
	Reserve(context.Context, ledger.ReservationRequest) (ledger.Order, bool, error)
	Resolve(context.Context, ledger.ResolutionRequest) (ledger.Order, bool, error)
	GetOrder(context.Context, string, string) (ledger.Order, error)
	Events(context.Context, string, int64, int) ([]ledger.Event, error)
}

type paperHTTP struct {
	store paperAccountStore
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
	var input ledger.CreateAccount
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
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	w.Header().Set("Location", "/v1/paper/accounts/"+account.ID)
	writeJSON(w, status, account)
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
	var input ledger.ReservationRequest
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
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	w.Header().Set("Location", "/v1/paper/accounts/"+order.AccountID+"/orders/"+order.OrderID)
	writeJSON(w, status, order)
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
	var input ledger.ResolutionRequest
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
	snapshot, err := papermemory.Load(ctx, h.store, r.PathValue("id"), asOf)
	if err != nil {
		writePaperError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func writePaperError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ledger.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_paper_request")
	case errors.Is(err, ledger.ErrNotFound), errors.Is(err, papermemory.ErrNoHistory):
		writeError(w, http.StatusNotFound, "paper_record_not_found")
	case errors.Is(err, ledger.ErrStale):
		writeError(w, http.StatusConflict, "paper_snapshot_stale")
	case errors.Is(err, ledger.ErrExpired):
		writeError(w, http.StatusConflict, "paper_quote_expired")
	case errors.Is(err, ledger.ErrInsufficient):
		writeError(w, http.StatusConflict, "paper_insufficient_funds")
	case errors.Is(err, ledger.ErrConflict):
		writeError(w, http.StatusConflict, "paper_conflict")
	default:
		writeError(w, http.StatusServiceUnavailable, "paper_storage_unavailable")
	}
}
