package dzengi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"tradingbot/venue-quotes/internal/quotes"
)

const orderBookURL = "https://marketcap.dzengi.com/api/v1/token/orderbook"
const maxResponseBytes = 256 * 1024

type Client struct {
	http *http.Client
}

func New() *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxResponseHeaderBytes = 16 * 1024
	return &Client{http: &http.Client{
		Transport:     transport,
		Timeout:       5 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (client *Client) CloseIdleConnections() {
	client.http.CloseIdleConnections()
}

type snapshot struct {
	Timestamp int64           `json:"timestamp"`
	Bids      [][]json.Number `json:"bids"`
	Asks      [][]json.Number `json:"asks"`
}

func (client *Client) OrderBook(ctx context.Context, request quotes.BookRequest) (quotes.OrderBook, error) {
	if err := request.Validate(); err != nil {
		return quotes.OrderBook{}, err
	}
	query := url.Values{"symbol": {request.Symbol}, "depth": {strconv.Itoa(request.Depth)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, orderBookURL+"?"+query.Encode(), nil)
	if err != nil {
		return quotes.OrderBook{}, quotes.ErrUnavailable
	}
	req.Header.Set("Accept", "application/json")
	requestedAt := time.Now().UTC()
	response, err := client.http.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return quotes.OrderBook{}, err
		}
		return quotes.OrderBook{}, quotes.ErrUnavailable
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusTooManyRequests, http.StatusTeapot:
		return quotes.OrderBook{}, quotes.ErrRateLimited
	default:
		return quotes.OrderBook{}, quotes.ErrUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return quotes.OrderBook{}, err
		}
		return quotes.OrderBook{}, quotes.ErrUnavailable
	}
	observedAt := time.Now().UTC()
	var book snapshot
	if len(body) > maxResponseBytes || json.Unmarshal(body, &book) != nil || book.Timestamp <= 0 {
		return quotes.OrderBook{}, quotes.ErrUnavailable
	}
	return quotes.NormalizeBook(quotes.OrderBook{
		BookRequest: request, Provider: "dzengi", Firmness: "indicative", ProviderTimestamp: book.Timestamp,
		RequestedAt: requestedAt, ObservedAt: observedAt,
	}, book.Bids, book.Asks)
}
