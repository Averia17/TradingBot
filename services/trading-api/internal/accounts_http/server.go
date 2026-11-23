package accounts_http

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"tradingbot/trading-api/internal/accounts"
	"tradingbot/trading-api/internal/contract"
	"tradingbot/trading-api/internal/trading_api"
)

// NewHandler exposes the public paper contract, the private worker calls and
// the optional isolated delivery endpoints of the account store.
func NewHandler(store *accounts.Store, token string, outbox http.Handler) http.Handler {
	mux := http.NewServeMux()
	paper := trading_api.NewHandler(time.Hour, time.Now, nil)
	if store != nil {
		paper = trading_api.NewHandlerWithPaper(time.Hour, time.Now, nil, store)
	}
	mux.Handle("GET /healthz", paper)
	guard := tokenGuard(token)
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if guard(w, r) {
			paper.ServeHTTP(w, r)
		}
	}))
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if store == nil || store.Ping(ctx) != nil {
			writeError(w, http.StatusServiceUnavailable, "paper_storage_unavailable")
			return
		}
		writeJSON(w, http.StatusOK, struct {
			Status         string `json:"status"`
			StorageEnabled bool   `json:"storage_enabled"`
		}{"ok", true})
	})
	if outbox != nil {
		mux.Handle(internalPath+"outbox/", outbox)
	}
	mux.Handle(internalPath, privateAccountHandler(store, token))
	return mux
}

func privateAccountHandler(store *accounts.Store, token string) http.Handler {
	routes := http.NewServeMux()
	if store != nil {
		routes.HandleFunc("POST "+internalPath+"reserve", jsonOperation(contract.PaperHTTPReservation, func(ctx context.Context, input accounts.ReservationRequest) (orderWrite, error) {
			order, recorded, err := store.Reserve(ctx, input)
			return orderWrite{Order: order, Recorded: recorded}, err
		}))
		routes.HandleFunc("POST "+internalPath+"resolve", jsonOperation(contract.PaperResolve, func(ctx context.Context, input accounts.ResolutionRequest) (orderWrite, error) {
			order, recorded, err := store.Resolve(ctx, input)
			return orderWrite{Order: order, Recorded: recorded}, err
		}))
		routes.HandleFunc("POST "+internalPath+"reconcile", jsonOperation(contract.PaperHTTPReconcile, func(ctx context.Context, input reconciliation) (orderWrite, error) {
			if !digestPattern.MatchString(input.SHA256) {
				return orderWrite{}, accounts.ErrInvalid
			}
			digest := sha256.Sum256(input.Observation)
			var observation accounts.ReconciliationObservation
			if len(input.Observation) > 256<<10 || input.SHA256 != hex.EncodeToString(digest[:]) ||
				contract.Decode(input.Observation, contract.PaperReconciliationObservation, &observation) != nil {
				return orderWrite{}, accounts.ErrInvalid
			}
			order, recorded, err := store.Reconcile(ctx, observation, digest)
			return orderWrite{Order: order, Recorded: recorded}, err
		}))
		routes.HandleFunc("GET "+internalPath+"applications/{job}", func(w http.ResponseWriter, r *http.Request) {
			record, err := store.GetApplication(r.Context(), r.PathValue("job"))
			writeResult(w, fromApplicationRecord(record), err)
		})
		routes.HandleFunc("GET "+internalPath+"accounts/{account}/applications/{job}", func(w http.ResponseWriter, r *http.Request) {
			record, err := store.ApplicationAfterReservations(r.Context(), r.PathValue("account"), r.PathValue("job"))
			writeResult(w, fromApplicationRecord(record), err)
		})
		routes.HandleFunc("POST "+internalPath+"applications/reserve", jsonOperation(contract.PaperHTTPReserveApplication, func(ctx context.Context, in reserveApplication) (orderWrite, error) {
			input, err := in.Application.domain()
			if err != nil {
				return orderWrite{}, err
			}
			order, recorded, err := store.ReserveApplication(ctx, in.Order, input)
			return orderWrite{Order: order, Recorded: recorded}, err
		}))
		routes.HandleFunc("POST "+internalPath+"applications/record", jsonOperation(contract.PaperHTTPRecordApplication, func(ctx context.Context, in recordApplication) (applicationRecord, error) {
			input, err := in.Application.domain()
			if err != nil {
				return applicationRecord{}, err
			}
			record, err := store.RecordApplication(ctx, input, in.OrderID, in.Result, in.ExpiresAt)
			return fromApplicationRecord(record), err
		}))
		routes.HandleFunc("GET "+internalPath+"accounts/{account}/risk-activity", func(w http.ResponseWriter, r *http.Request) {
			version, err := strconv.ParseInt(r.URL.Query().Get("version"), 10, 64)
			if err != nil {
				writeResult(w, nil, accounts.ErrInvalid)
				return
			}
			activity, err := store.RiskActivity(r.Context(), r.PathValue("account"), version)
			writeResult(w, activity, err)
		})
		routes.HandleFunc("POST "+internalPath+"risk-refusals", jsonOperation(contract.PaperHTTPRiskRefusal, func(ctx context.Context, in riskRefusal) (struct{}, error) {
			return struct{}{}, store.RecordRiskRefusal(ctx, in.domain())
		}))
	}
	guard := tokenGuard(token)
	inFlight := make(chan struct{}, 4)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		select {
		case inFlight <- struct{}{}:
			defer func() { <-inFlight }()
		default:
			writeError(w, http.StatusServiceUnavailable, "paper_api_busy")
			return
		}
		if store == nil {
			writeError(w, http.StatusServiceUnavailable, "paper_storage_unavailable")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout-time.Second)
		defer cancel()
		routes.ServeHTTP(w, r.WithContext(ctx))
	})
}

func tokenGuard(token string) func(http.ResponseWriter, *http.Request) bool {
	configured := validToken(token)
	expected := sha256.Sum256([]byte("Bearer " + token))
	return func(w http.ResponseWriter, r *http.Request) bool {
		if !configured {
			writeError(w, http.StatusNotFound, "paper_internal_disabled")
			return false
		}
		provided := sha256.Sum256([]byte(r.Header.Get("Authorization")))
		if subtle.ConstantTimeCompare(provided[:], expected[:]) != 1 {
			writeError(w, http.StatusUnauthorized, "paper_internal_unauthorized")
			return false
		}
		return true
	}
}

func jsonOperation[I, O any](schema *jsonschema.Schema, run func(context.Context, I) (O, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mediaType != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, "unsupported_content_type")
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large")
			return
		}
		var input I
		if err != nil || !utf8.Valid(raw) || contract.Decode(raw, schema, &input) != nil {
			writeError(w, http.StatusBadRequest, "invalid_paper_request")
			return
		}
		result, err := run(r.Context(), input)
		writeResult(w, result, err)
	}
}

func writeResult(w http.ResponseWriter, result any, err error) {
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, result)
	case errors.Is(err, accounts.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_paper_request")
	case errors.Is(err, accounts.ErrNotFound):
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

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

func writeJSON(w http.ResponseWriter, status int, result any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(result)
}
