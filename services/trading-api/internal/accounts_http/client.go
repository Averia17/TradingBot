package accounts_http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"tradingbot/trading-api/internal/accounts"
	"tradingbot/trading-api/internal/contract"
)

type Client struct {
	origin string
	token  string
	http   *http.Client
}

func NewClient(origin, token string) (*Client, error) {
	base, err := url.Parse(origin)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") ||
		base.Hostname() == "" || base.User != nil || base.Opaque != "" ||
		strings.ContainsAny(origin, "?#") || (base.EscapedPath() != "" && base.EscapedPath() != "/") {
		return nil, errors.New("paper accounts URL must be an HTTP origin")
	}
	if !validToken(token) {
		return nil, errors.New("paper accounts HTTP requires a valid internal token")
	}
	return &Client{
		origin: strings.TrimSuffix(base.String(), "/"), token: token,
		http: &http.Client{
			Timeout: requestTimeout,
			Transport: &http.Transport{
				DialContext:            (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				TLSHandshakeTimeout:    3 * time.Second,
				IdleConnTimeout:        30 * time.Second,
				MaxIdleConnsPerHost:    4,
				MaxResponseHeaderBytes: 8 << 10,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func (c *Client) Close() { c.http.CloseIdleConnections() }

func (c *Client) Ping(ctx context.Context) error {
	var readiness struct {
		Status         string `json:"status"`
		StorageEnabled bool   `json:"storage_enabled"`
	}
	_, err := c.call(ctx, http.MethodGet, "/readyz", nil, &readiness, nil)
	if err != nil {
		return err
	}
	if readiness.Status != "ok" || !readiness.StorageEnabled {
		return errors.New("paper accounts storage is not ready")
	}
	return nil
}

func (c *Client) CreateAccount(ctx context.Context, input accounts.CreateAccount) (accounts.Account, bool, error) {
	var account accounts.Account
	status, err := c.call(ctx, http.MethodPost, "/v1/paper/accounts", input, &account, contract.PaperHTTPAccount)
	return account, status == http.StatusCreated, err
}

func (c *Client) GetAccount(ctx context.Context, accountID string) (accounts.Account, error) {
	var account accounts.Account
	_, err := c.call(ctx, http.MethodGet, accountPath(accountID), nil, &account, contract.PaperHTTPAccount)
	return account, err
}

func (c *Client) GetOrder(ctx context.Context, accountID, orderID string) (accounts.Order, error) {
	var order accounts.Order
	_, err := c.call(ctx, http.MethodGet, accountPath(accountID)+"/orders/"+url.PathEscape(orderID), nil, &order, contract.PaperHTTPOrder)
	return order, err
}

func accountPath(accountID string) string { return "/v1/paper/accounts/" + url.PathEscape(accountID) }

func (c *Client) Events(ctx context.Context, accountID string, afterVersion int64, limit int) ([]accounts.Event, error) {
	var events []accounts.Event
	query := url.Values{"after_version": {strconv.FormatInt(afterVersion, 10)}, "limit": {strconv.Itoa(limit)}}
	_, err := c.call(ctx, http.MethodGet, accountPath(accountID)+"/events?"+query.Encode(), nil, &events, nil)
	return events, err
}

func (c *Client) Reserve(ctx context.Context, input accounts.ReservationRequest) (accounts.Order, bool, error) {
	return c.writeOrder(ctx, "reserve", input)
}

func (c *Client) Resolve(ctx context.Context, input accounts.ResolutionRequest) (accounts.Order, bool, error) {
	return c.writeOrder(ctx, "resolve", input)
}

func (c *Client) Reconcile(ctx context.Context, observation accounts.ReconciliationObservation, digest [sha256.Size]byte) (accounts.Order, bool, error) {
	raw, err := json.Marshal(observation)
	if err != nil || sha256.Sum256(raw) != digest {
		return accounts.Order{}, false, accounts.ErrInvalid
	}
	return c.ReconcileDocument(ctx, raw, digest)
}

func (c *Client) ReconcileDocument(ctx context.Context, observation []byte, digest [sha256.Size]byte) (accounts.Order, bool, error) {
	return c.writeOrder(ctx, "reconcile", reconciliation{Observation: observation, SHA256: hex.EncodeToString(digest[:])})
}

func (c *Client) ReserveApplication(ctx context.Context, order accounts.ReservationRequest, input accounts.ApplicationInput) (accounts.Order, bool, error) {
	return c.writeOrder(ctx, "applications/reserve", reserveApplication{Order: order, Application: fromApplication(input)})
}

func (c *Client) writeOrder(ctx context.Context, operation string, input any) (accounts.Order, bool, error) {
	var receipt orderWrite
	_, err := c.call(ctx, http.MethodPost, internalPath+operation, input, &receipt, contract.PaperHTTPOrderWrite)
	return receipt.Order, receipt.Recorded, err
}

func (c *Client) GetApplication(ctx context.Context, jobID string) (accounts.ApplicationRecord, error) {
	return c.application(ctx, http.MethodGet, "applications/"+url.PathEscape(jobID), nil)
}

func (c *Client) ApplicationAfterReservations(ctx context.Context, accountID, jobID string) (accounts.ApplicationRecord, error) {
	return c.application(ctx, http.MethodGet, "accounts/"+url.PathEscape(accountID)+"/applications/"+url.PathEscape(jobID), nil)
}

func (c *Client) RecordApplication(ctx context.Context, input accounts.ApplicationInput, orderID string, result []byte, expiresAt time.Time) (accounts.ApplicationRecord, error) {
	return c.application(ctx, http.MethodPost, "applications/record",
		recordApplication{Application: fromApplication(input), OrderID: orderID, Result: result, ExpiresAt: expiresAt})
}

func (c *Client) application(ctx context.Context, method, path string, input any) (accounts.ApplicationRecord, error) {
	var receipt applicationRecord
	if _, err := c.call(ctx, method, internalPath+path, input, &receipt, contract.PaperHTTPApplicationRecord); err != nil {
		return accounts.ApplicationRecord{}, err
	}
	return receipt.domain()
}

func (c *Client) RiskActivity(ctx context.Context, accountID string, version int64) (accounts.RiskActivity, error) {
	var activity accounts.RiskActivity
	path := "accounts/" + url.PathEscape(accountID) + "/risk-activity?version=" + strconv.FormatInt(version, 10)
	_, err := c.call(ctx, http.MethodGet, internalPath+path, nil, &activity, contract.PaperHTTPRiskActivity)
	return activity, err
}

func (c *Client) RecordRiskRefusal(ctx context.Context, input accounts.RiskRefusalInput) error {
	var receipt struct{}
	_, err := c.call(ctx, http.MethodPost, internalPath+"risk-refusals", fromRiskRefusal(input), &receipt, nil)
	return err
}

func (c *Client) call(ctx context.Context, method, path string, input, result any, schema *jsonschema.Schema) (int, error) {
	var body io.Reader
	if input != nil {
		raw, err := json.Marshal(input)
		if err != nil || len(raw) > maxRequestBody {
			return 0, accounts.ErrInvalid
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.origin+path, body)
	if err != nil {
		return 0, accounts.ErrInvalid
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.GetBody = nil
	}
	if path != "/readyz" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, errors.New("paper accounts HTTP is unavailable")
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBody+1))
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || len(raw) > maxResponseBody || mediaType != "application/json" {
		return response.StatusCode, errors.New("invalid response from paper accounts")
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return response.StatusCode, responseError(response.StatusCode, raw)
	}
	if contract.Decode(raw, schema, result) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return response.StatusCode, errors.New("invalid response from paper accounts")
	}
	return response.StatusCode, nil
}

func responseError(status int, raw []byte) error {
	var response struct {
		Error string `json:"error"`
	}
	if contract.Decode(raw, nil, &response) == nil {
		switch status {
		case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType:
			return accounts.ErrInvalid
		case http.StatusNotFound:
			if response.Error == "paper_record_not_found" {
				return accounts.ErrNotFound
			}
		case http.StatusConflict:
			switch response.Error {
			case "paper_snapshot_stale":
				return accounts.ErrStale
			case "paper_quote_expired":
				return accounts.ErrExpired
			case "paper_insufficient_funds":
				return accounts.ErrInsufficient
			case "paper_conflict":
				return accounts.ErrConflict
			}
		}
	}
	return fmt.Errorf("paper accounts HTTP returned status %d", status)
}
