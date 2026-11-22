package jupiter_http

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"tradingbot/venue-quotes/internal/jupiter_execution"
	"tradingbot/venue-quotes/internal/solana_balance"
)

const (
	maxExecutionBodyBytes = 8192
	invalidExecutionCode  = "invalid_execution_request"
	invalidStatusCode     = "invalid_transaction_status_request"
)

type ChainStatusProvider interface {
	TransactionStatus(context.Context, string) (solana_balance.TransactionStatus, error)
}

type SubmissionService interface {
	Submit(context.Context, jupiter_execution.Request) (jupiter_execution.Receipt, error)
	Receipt(context.Context, string) (jupiter_execution.Receipt, error)
}

// RegisterTransactions adds public chain observations and private execution routes.
func RegisterTransactions(mux *http.ServeMux, chain ChainStatusProvider, executor SubmissionService, token string) {
	executionSlots := make(chan struct{}, maxConcurrent)
	statusSlots := make(chan struct{}, maxConcurrent)
	credential := sha256.Sum256([]byte(token))
	enabled := executor != nil && token != ""
	mux.Handle("POST /v1/swaps/execute", privateExecution(enabled, credential, limited(executionSlots, executeHandler(executor))))
	mux.Handle("GET /v1/swaps/attempts/{intent_id}", privateExecution(enabled, credential, limited(executionSlots, receiptHandler(executor))))
	mux.Handle("GET /v1/swaps/status", limited(statusSlots, transactionStatusHandler(chain)))
}

func privateExecution(enabled bool, credential [sha256.Size]byte, handle http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !enabled {
			writeError(w, http.StatusServiceUnavailable, "swap_execution_disabled")
			return
		}
		if !validBearer(r.Header.Values("Authorization"), credential) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "executor_authentication_required")
			return
		}
		handle(w, r)
	}
}

func validBearer(headers []string, credential [sha256.Size]byte) bool {
	if len(headers) != 1 {
		return false
	}
	scheme, value, ok := strings.Cut(headers[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || value == "" {
		return false
	}
	provided := sha256.Sum256([]byte(value))
	return subtle.ConstantTimeCompare(provided[:], credential[:]) == 1
}

func executeHandler(executor SubmissionService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			writeError(w, http.StatusBadRequest, invalidExecutionCode)
			return
		}
		if mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mediaType != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, "expected_json")
			return
		}
		request, err := readExecutionRequest(http.MaxBytesReader(w, r.Body, maxExecutionBodyBytes))
		if err != nil {
			writeBodyError(w, err, invalidExecutionCode)
			return
		}
		receipt, err := executor.Submit(r.Context(), request)
		writeExecutionResponse(w, http.StatusAccepted, receipt, err)
	}
}

func readExecutionRequest(body io.Reader) (jupiter_execution.Request, error) {
	var request jupiter_execution.Request
	fields := map[string]any{
		"approval": &request.Approval, "approval_signature": &request.ApprovalSignature,
		"signed_transaction_base64": &request.SignedTransaction,
	}
	if err := decodeObject(body, fields); err != nil {
		return jupiter_execution.Request{}, err
	}
	if request.Approval == "" || request.ApprovalSignature == "" || request.SignedTransaction == "" {
		return jupiter_execution.Request{}, errInvalidObject
	}
	return request, nil
}

func receiptHandler(executor SubmissionService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			writeError(w, http.StatusBadRequest, invalidExecutionCode)
			return
		}
		receipt, err := executor.Receipt(r.Context(), r.PathValue("intent_id"))
		writeExecutionResponse(w, http.StatusOK, receipt, err)
	}
}

func transactionStatusHandler(chain ChainStatusProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params, err := url.ParseQuery(r.URL.RawQuery)
		signatures := params["signature"]
		if err != nil || len(params) != 1 || len(signatures) != 1 || signatures[0] == "" {
			writeError(w, http.StatusBadRequest, invalidStatusCode)
			return
		}
		if chain == nil {
			writeError(w, http.StatusServiceUnavailable, "transaction_status_not_configured")
			return
		}
		status, err := chain.TransactionStatus(r.Context(), signatures[0])
		if errors.Is(err, solana_balance.ErrInvalidSignature) {
			writeError(w, http.StatusBadRequest, invalidStatusCode)
			return
		}
		writeProviderResponse(w, status, err, invalidStatusCode)
	}
}

func writeExecutionResponse(w http.ResponseWriter, successStatus int, receipt jupiter_execution.Receipt, err error) {
	switch {
	case err == nil:
		writeJSON(w, successStatus, receipt)
	case errors.Is(err, jupiter_execution.ErrInvalid):
		writeError(w, http.StatusBadRequest, invalidExecutionCode)
	case errors.Is(err, jupiter_execution.ErrUnauthorized):
		writeError(w, http.StatusForbidden, "swap_approval_rejected")
	case errors.Is(err, jupiter_execution.ErrConflict):
		writeError(w, http.StatusConflict, "swap_attempt_conflict")
	case errors.Is(err, jupiter_execution.ErrExpired):
		writeError(w, http.StatusUnprocessableEntity, "swap_approval_expired")
	case errors.Is(err, jupiter_execution.ErrNotFound):
		writeError(w, http.StatusNotFound, "swap_attempt_not_found")
	case errors.Is(err, jupiter_execution.ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, "swap_execution_unavailable")
	default:
		writeError(w, http.StatusInternalServerError, "swap_execution_unavailable")
	}
}
