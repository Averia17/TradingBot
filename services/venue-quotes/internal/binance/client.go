package binance

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

const orderBookURL = "https://data-api.binance.vision/api/v3/depth"
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
	LastUpdateID int64           `json:"lastUpdateId"`
	Bids         [][]json.Number `json:"bids"`
	Asks         [][]json.Number `json:"asks"`
	Code         int             `json:"code"`
}

func (client *Client) OrderBook(ctx context.Context, request quotes.BookRequest) (quotes.OrderBook, error) {
	if err := request.Validate(); err != nil {
		return quotes.OrderBook{}, err
	}
	query := url.Values{"symbol": {request.Symbol}, "limit": {strconv.Itoa(request.Depth)}}
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
	case http.StatusOK, http.StatusBadRequest:
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
	if len(body) > maxResponseBytes || json.Unmarshal(body, &book) != nil {
		return quotes.OrderBook{}, quotes.ErrUnavailable
	}
	if response.StatusCode == http.StatusBadRequest && book.Code == -1121 {
		return quotes.OrderBook{}, quotes.ErrNoOrderBook
	}
	if response.StatusCode != http.StatusOK || book.Code != 0 || book.LastUpdateID <= 0 {
		return quotes.OrderBook{}, quotes.ErrUnavailable
	}
	return quotes.NormalizeBook(quotes.OrderBook{
		BookRequest: request, Provider: "binance", Firmness: "indicative", ProviderSequence: book.LastUpdateID,
		RequestedAt: requestedAt, ObservedAt: observedAt,
	}, book.Bids, book.Asks)
}
