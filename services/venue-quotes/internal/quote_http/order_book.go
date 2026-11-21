package quote_http

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"tradingbot/venue-quotes/internal/quotes"
)

type OrderBookProvider interface {
	OrderBook(context.Context, quotes.BookRequest) (quotes.OrderBook, error)
}

func RegisterOrderBook(mux *http.ServeMux, path string, provider OrderBookProvider) {
	slots := make(chan struct{}, maxConcurrentQuotes)
	mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		request, ok := readBookRequest(r.URL.RawQuery)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_order_book_request")
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			writeError(w, http.StatusServiceUnavailable, "quote_service_busy")
			return
		}
		book, err := provider.OrderBook(r.Context(), request)
		writeProviderResponse(w, book, err)
	})
}

func readBookRequest(rawQuery string) (quotes.BookRequest, bool) {
	if len(rawQuery) > 128 {
		return quotes.BookRequest{}, false
	}
	query, err := url.ParseQuery(rawQuery)
	if err != nil || len(query["symbol"]) != 1 {
		return quotes.BookRequest{}, false
	}
	request := quotes.BookRequest{Symbol: query.Get("symbol"), Depth: 5}
	delete(query, "symbol")
	if depths, supplied := query["depth"]; supplied {
		if len(depths) != 1 {
			return quotes.BookRequest{}, false
		}
		depth, err := strconv.Atoi(depths[0])
		if err != nil || strconv.Itoa(depth) != depths[0] {
			return quotes.BookRequest{}, false
		}
		request.Depth = depth
		delete(query, "depth")
	}
	return request, len(query) == 0 && request.Validate() == nil
}
