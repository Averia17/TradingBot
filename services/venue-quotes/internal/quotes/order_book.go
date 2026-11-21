package quotes

import (
	"errors"
	"regexp"
	"time"
)

const MaxBookDepth = 500

var (
	ErrNoOrderBook = errors.New("provider returned no order book")
	marketSymbol   = regexp.MustCompile(`^[A-Z][A-Z0-9.-]{0,15}$`)
)

type BookRequest struct {
	Symbol string `json:"symbol"`
	Depth  int    `json:"depth"`
}

func (request BookRequest) Validate() error {
	if !marketSymbol.MatchString(request.Symbol) {
		return errors.New("invalid token market symbol")
	}
	switch request.Depth {
	case 5, 10, 20, 50, 100, MaxBookDepth:
		return nil
	default:
		return errors.New("unsupported order book depth")
	}
}

type BookLevel struct {
	Price    string `json:"price"`
	Quantity string `json:"quantity"`
}

type OrderBook struct {
	BookRequest
	Provider          string      `json:"provider"`
	Firmness          string      `json:"firmness"`
	ProviderTimestamp int64       `json:"provider_timestamp,omitempty"`
	ProviderSequence  int64       `json:"provider_sequence,omitempty"`
	RequestedAt       time.Time   `json:"requested_at"`
	ObservedAt        time.Time   `json:"observed_at"`
	Bids              []BookLevel `json:"bids"`
	Asks              []BookLevel `json:"asks"`
}
