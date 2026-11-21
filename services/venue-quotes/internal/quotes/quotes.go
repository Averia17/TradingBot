package quotes

import (
	"errors"
	"regexp"
	"strconv"
	"time"
)

const SolanaMainnet = "solana-mainnet"

var (
	ErrRateLimited = errors.New("quote provider rate limited")
	ErrUnavailable = errors.New("quote provider unavailable")
	mintPattern    = regexp.MustCompile(`^[1-9A-HJ-NP-Za-km-z]{32,44}$`)
)

type Request struct {
	Network    string `json:"network"`
	InputMint  string `json:"input_mint"`
	OutputMint string `json:"output_mint"`
	InAmount   string `json:"in_amount"`
}

func (request Request) Validate() error {
	if request.Network != SolanaMainnet {
		return errors.New("unsupported network")
	}
	if !ValidMint(request.InputMint) || !ValidMint(request.OutputMint) || request.InputMint == request.OutputMint {
		return errors.New("provide two distinct Solana mint addresses")
	}
	if _, ok := PositiveAmount(request.InAmount); !ok {
		return errors.New("in_amount must be a positive uint64 string in raw input units")
	}
	return nil
}

type Observation struct {
	Request
	Provider          string     `json:"provider"`
	Firmness          string     `json:"firmness"`
	OutAmount         string     `json:"out_amount"`
	MinOutAmount      string     `json:"min_out_amount"`
	SwapFeeBPS        *uint16    `json:"swap_fee_bps"`
	SwapFeeMint       *string    `json:"swap_fee_mint"`
	Route             string     `json:"route"`
	ProviderRequestID string     `json:"provider_request_id"`
	RequestedAt       time.Time  `json:"requested_at"`
	ObservedAt        time.Time  `json:"observed_at"`
	ProviderExpiresAt *time.Time `json:"provider_expires_at"`
}

func ValidMint(value string) bool {
	return mintPattern.MatchString(value)
}

func PositiveAmount(value string) (uint64, bool) {
	amount, err := strconv.ParseUint(value, 10, 64)
	return amount, err == nil && amount > 0 && strconv.FormatUint(amount, 10) == value
}
