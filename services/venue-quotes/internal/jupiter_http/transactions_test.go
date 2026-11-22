package jupiter_http

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tradingbot/venue-quotes/internal/jupiter_execution"
	"tradingbot/venue-quotes/internal/solana_balance"
)

const (
	testCredential = "executor-service-token"
	testIntent     = "0aab064e-f8f7-4eec-930c-c9bff7755786"
	executionBody  = `{"approval":"YWJj","approval_signature":"ZGVm","signed_transaction_base64":"Z2hp"}`
)

type testChainStatus func(context.Context, string) (solana_balance.TransactionStatus, error)

func (s testChainStatus) TransactionStatus(ctx context.Context, signature string) (solana_balance.TransactionStatus, error) {
	return s(ctx, signature)
}

type testExecution struct {
	submit  func(context.Context, jupiter_execution.Request) (jupiter_execution.Receipt, error)
	receipt func(context.Context, string) (jupiter_execution.Receipt, error)
}

func (e testExecution) Submit(ctx context.Context, input jupiter_execution.Request) (jupiter_execution.Receipt, error) {
	return e.submit(ctx, input)
}

func (e testExecution) Receipt(ctx context.Context, intentID string) (jupiter_execution.Receipt, error) {
	return e.receipt(ctx, intentID)
}

func transactionRequest(t *testing.T, handler http.Handler, method, path, body, contentType string, auth []string, code int, expected string) {
	t.Helper()
	request := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	for _, value := range auth {
		request.Header.Add("Authorization", value)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != code || !strings.Contains(response.Body.String(), expected) {
		t.Fatalf("%s %s: status=%d response=%s", method, path, response.Code, response.Body)
	}
	if response.Header().Get("Cache-Control") != "no-store" || strings.Contains(response.Body.String(), testCredential) {
		t.Fatalf("response must be private and omit credentials: headers=%v body=%s", response.Header(), response.Body)
	}
	if code == http.StatusUnauthorized && response.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("missing bearer challenge: headers=%v", response.Header())
	}
}

func TestRegisterTransactionsRoutes(t *testing.T) {
	const signature = "status-test-signature"
	chain := testChainStatus(func(_ context.Context, value string) (solana_balance.TransactionStatus, error) {
		if value != signature {
			t.Fatalf("unexpected signature %q", value)
		}
		return solana_balance.TransactionStatus{}, nil
	})
	executor := testExecution{
		submit: func(ctx context.Context, input jupiter_execution.Request) (jupiter_execution.Receipt, error) {
			if _, bounded := ctx.Deadline(); !bounded || input != (jupiter_execution.Request{Approval: "YWJj", ApprovalSignature: "ZGVm", SignedTransaction: "Z2hp"}) {
				t.Fatalf("submit bounded=%t input=%+v", bounded, input)
			}
			return jupiter_execution.Receipt{}, nil
		},
		receipt: func(_ context.Context, intentID string) (jupiter_execution.Receipt, error) {
			if intentID != testIntent {
				t.Fatalf("unexpected intent ID %q", intentID)
			}
			return jupiter_execution.Receipt{}, nil
		},
	}
	handler := Handler(testMarket{}, nil, "")
	RegisterTransactions(handler, chain, executor, testCredential)
	auth := []string{"Bearer " + testCredential}
	transactionRequest(t, handler, http.MethodGet, "/v1/swaps/status?signature="+signature, "", "", nil, http.StatusOK, "{")
	transactionRequest(t, handler, http.MethodPost, "/v1/swaps/execute", executionBody, "application/json", auth, http.StatusAccepted, "{")
	transactionRequest(t, handler, http.MethodGet, "/v1/swaps/attempts/"+testIntent, "", "", auth, http.StatusOK, "{")
}

func TestDisabledAndUnauthorizedExecutionRoutes(t *testing.T) {
	for _, config := range []struct {
		executor SubmissionService
		token    string
	}{
		{nil, testCredential}, {testExecution{}, ""},
	} {
		handler := http.NewServeMux()
		RegisterTransactions(handler, nil, config.executor, config.token)
		transactionRequest(t, handler, http.MethodPost, "/v1/swaps/execute", executionBody, "application/json", nil, http.StatusServiceUnavailable, "swap_execution_disabled")
		transactionRequest(t, handler, http.MethodGet, "/v1/swaps/attempts/"+testIntent, "", "", nil, http.StatusServiceUnavailable, "swap_execution_disabled")
		transactionRequest(t, handler, http.MethodGet, "/v1/swaps/status?signature=test", "", "", nil, http.StatusServiceUnavailable, "transaction_status_not_configured")
	}
	handler := http.NewServeMux()
	RegisterTransactions(handler, nil, testExecution{}, testCredential)
	for _, auth := range [][]string{
		nil, {"Basic " + testCredential}, {"Bearer invalid"}, {"Bearer " + testCredential + ","}, {"Bearer " + testCredential, "Bearer " + testCredential},
	} {
		transactionRequest(t, handler, http.MethodPost, "/v1/swaps/execute", executionBody, "application/json", auth, http.StatusUnauthorized, "executor_authentication_required")
		transactionRequest(t, handler, http.MethodGet, "/v1/swaps/attempts/"+testIntent, "", "", auth, http.StatusUnauthorized, "executor_authentication_required")
	}
}

func TestExecutionRequestDecoding(t *testing.T) {
	handler := http.NewServeMux()
	RegisterTransactions(handler, testChainStatus(nil), testExecution{}, testCredential)
	auth := []string{"Bearer " + testCredential}
	for _, body := range []string{
		`[]`,
		`{"approval":"YWJj","approval_signature":"ZGVm"}`,
		strings.Replace(executionBody, `"ZGVm"`, "null", 1),
		strings.Replace(executionBody, `"ZGVm"`, "123", 1),
		strings.Replace(executionBody, `"ZGVm"`, `""`, 1),
		strings.Replace(executionBody, `"approval":"YWJj"`, `"approval":"YWJj","approval":"duplicate"`, 1),
		strings.Replace(executionBody, `"approval":"YWJj"`, `"unknown":"YWJj"`, 1),
		executionBody + "{}",
	} {
		transactionRequest(t, handler, http.MethodPost, "/v1/swaps/execute", body, "application/json", auth, http.StatusBadRequest, "invalid_execution_request")
	}
	transactionRequest(t, handler, http.MethodPost, "/v1/swaps/execute?owner=other", executionBody, "application/json", auth, http.StatusBadRequest, "invalid_execution_request")
	transactionRequest(t, handler, http.MethodPost, "/v1/swaps/execute", executionBody, "text/plain", auth, http.StatusUnsupportedMediaType, "expected_json")
	transactionRequest(t, handler, http.MethodPost, "/v1/swaps/execute", executionBody+strings.Repeat(" ", 8192), "application/json", auth, http.StatusRequestEntityTooLarge, "request_too_large")
	transactionRequest(t, handler, http.MethodGet, "/v1/swaps/attempts/"+testIntent+"?owner=other", "", "", auth, http.StatusBadRequest, "invalid_execution_request")
	for _, path := range []string{
		"/v1/swaps/status", "/v1/swaps/status?signature=", "/v1/swaps/status?signature=test&signature=test", "/v1/swaps/status?signature=test&rpc=test",
	} {
		transactionRequest(t, handler, http.MethodGet, path, "", "", nil, http.StatusBadRequest, "invalid_transaction_status_request")
	}
}

func TestExecutionAndChainErrorsStayOpaque(t *testing.T) {
	for _, result := range []struct {
		err    error
		status int
		code   string
	}{
		{jupiter_execution.ErrInvalid, http.StatusBadRequest, "invalid_execution_request"},
		{jupiter_execution.ErrUnauthorized, http.StatusForbidden, "swap_approval_rejected"},
		{jupiter_execution.ErrConflict, http.StatusConflict, "swap_attempt_conflict"},
		{jupiter_execution.ErrExpired, http.StatusUnprocessableEntity, "swap_approval_expired"},
		{jupiter_execution.ErrNotFound, http.StatusNotFound, "swap_attempt_not_found"},
		{jupiter_execution.ErrUnavailable, http.StatusServiceUnavailable, "swap_execution_unavailable"},
	} {
		failure := errors.Join(result.err, errors.New(testCredential))
		execution := testExecution{
			submit: func(context.Context, jupiter_execution.Request) (jupiter_execution.Receipt, error) {
				return jupiter_execution.Receipt{}, failure
			},
			receipt: func(context.Context, string) (jupiter_execution.Receipt, error) {
				return jupiter_execution.Receipt{}, failure
			},
		}
		handler := http.NewServeMux()
		RegisterTransactions(handler, nil, execution, testCredential)
		auth := []string{"Bearer " + testCredential}
		transactionRequest(t, handler, http.MethodPost, "/v1/swaps/execute", executionBody, "application/json", auth, result.status, result.code)
		transactionRequest(t, handler, http.MethodGet, "/v1/swaps/attempts/"+testIntent, "", "", auth, result.status, result.code)
	}

	for _, result := range []struct {
		err    error
		status int
		code   string
	}{
		{solana_balance.ErrInvalidSignature, http.StatusBadRequest, "invalid_transaction_status_request"},
		{solana_balance.ErrUnavailable, http.StatusBadGateway, "provider_unavailable"},
		{context.DeadlineExceeded, http.StatusGatewayTimeout, "provider_timeout"},
	} {
		handler := http.NewServeMux()
		RegisterTransactions(handler, testChainStatus(func(context.Context, string) (solana_balance.TransactionStatus, error) {
			return solana_balance.TransactionStatus{}, errors.Join(result.err, errors.New(testCredential))
		}), nil, "")
		transactionRequest(t, handler, http.MethodGet, "/v1/swaps/status?signature=test", "", "", nil, result.status, result.code)
	}
}
