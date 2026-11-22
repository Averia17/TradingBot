package jupiter

import (
	"context"
	"encoding/base64"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"tradingbot/venue-quotes/internal/quotes"
	"tradingbot/venue-quotes/internal/solana_balance"
)

const nativeUSDC = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
const maxV0TransactionBytes = 1232

var (
	ErrInvalidRequest = errors.New("invalid Jupiter adapter request")
	ErrMinimumOutput  = errors.New("Jupiter minimum output is below the requested minimum")
)

type SwapRequest struct {
	Side         string `json:"side"`
	AssetMint    string `json:"asset_mint"`
	InAmount     string `json:"in_amount"`
	MinOutAmount string `json:"min_out_amount"`
	SlippageBPS  uint16 `json:"slippage_bps"`
}

type PreparedSwap struct {
	SchemaVersion         string     `json:"schema_version"`
	State                 string     `json:"state"`
	Network               string     `json:"network"`
	Owner                 string     `json:"owner"`
	Side                  string     `json:"side"`
	AssetMint             string     `json:"asset_mint"`
	InputMint             string     `json:"input_mint"`
	OutputMint            string     `json:"output_mint"`
	InAmount              string     `json:"in_amount"`
	OutAmount             string     `json:"out_amount"`
	MinOutAmount          string     `json:"min_out_amount"`
	RequestedMinOutAmount string     `json:"requested_min_out_amount"`
	SlippageBPS           uint16     `json:"slippage_bps"`
	SwapFeeBPS            *uint16    `json:"swap_fee_bps"`
	SwapFeeMint           *string    `json:"swap_fee_mint"`
	SignatureFeeLamports  string     `json:"signature_fee_lamports"`
	PriorityFeeLamports   string     `json:"priority_fee_lamports"`
	RentFeeLamports       string     `json:"rent_fee_lamports"`
	Router                string     `json:"router"`
	ProviderRequestID     string     `json:"provider_request_id"`
	TransactionVersion    uint8      `json:"transaction_version"`
	TransactionBase64     string     `json:"transaction_base64"`
	LastValidBlockHeight  string     `json:"last_valid_block_height"`
	RequestedAt           time.Time  `json:"requested_at"`
	ObservedAt            time.Time  `json:"observed_at"`
	ProviderExpiresAt     *time.Time `json:"provider_expires_at"`
}

type preparedOrder struct {
	orderResponse
	SlippageBPS          *uint16 `json:"slippageBps"`
	TransactionVersion   *uint8  `json:"transactionVersion"`
	LastValidBlockHeight string  `json:"lastValidBlockHeight"`
	Gasless              *bool   `json:"gasless"`
	SignatureFeePayer    *string `json:"signatureFeePayer"`
	PriorityFeePayer     *string `json:"prioritizationFeePayer"`
	RentFeePayer         *string `json:"rentFeePayer"`
	SignatureFeeLamports *uint64 `json:"signatureFeeLamports"`
	PriorityFeeLamports  *uint64 `json:"prioritizationFeeLamports"`
	RentFeeLamports      *uint64 `json:"rentFeeLamports"`
}

// PrepareSwap fetches a Jupiter transaction candidate for independent verification.
func (client *Client) PrepareSwap(ctx context.Context, owner string, request SwapRequest) (PreparedSwap, error) {
	if !solana_balance.ValidAddress(owner) || !solana_balance.ValidAddress(request.AssetMint) ||
		request.AssetMint == nativeUSDC || request.SlippageBPS > 10_000 {
		return PreparedSwap{}, ErrInvalidRequest
	}
	requiredMinimum, validMinimum := quotes.PositiveAmount(request.MinOutAmount)
	if _, valid := quotes.PositiveAmount(request.InAmount); !valid || !validMinimum {
		return PreparedSwap{}, ErrInvalidRequest
	}
	pair := quotes.Request{Network: quotes.SolanaMainnet, InputMint: nativeUSDC, OutputMint: request.AssetMint, InAmount: request.InAmount}
	switch request.Side {
	case "BUY":
	case "SELL":
		pair.InputMint, pair.OutputMint = pair.OutputMint, pair.InputMint
	default:
		return PreparedSwap{}, ErrInvalidRequest
	}
	params := url.Values{
		"inputMint": {pair.InputMint}, "outputMint": {pair.OutputMint}, "amount": {pair.InAmount},
		"taker": {owner}, "swapMode": {"ExactIn"}, "slippageBps": {strconv.FormatUint(uint64(request.SlippageBPS), 10)},
		"excludeRouters": {"jupiterz,dflow,okx"}, "maxSupportedTransactionVersion": {"0"},
	}
	var order preparedOrder
	requestedAt, observedAt, err := client.fetchJSON(ctx, orderPath, params, &order)
	if err != nil {
		return PreparedSwap{}, err
	}
	minimum, validQuote := order.marketMinimum(pair, requestedAt, observedAt)
	_, validHeight := quotes.PositiveAmount(order.LastValidBlockHeight)
	if !validQuote || order.Taker == nil || *order.Taker != owner || order.Router != "metis" ||
		order.SlippageBPS == nil || *order.SlippageBPS != request.SlippageBPS ||
		order.TransactionVersion == nil || *order.TransactionVersion != 0 || !validHeight ||
		order.Gasless == nil || *order.Gasless || !order.validFees(owner) ||
		order.Transaction == nil || !validTransactionEncoding(*order.Transaction) {
		return PreparedSwap{}, quotes.ErrUnavailable
	}
	if minimum < requiredMinimum {
		return PreparedSwap{}, ErrMinimumOutput
	}
	return PreparedSwap{
		SchemaVersion: "1", State: "prepared", Network: pair.Network, Owner: owner, Side: request.Side,
		AssetMint: request.AssetMint, InputMint: pair.InputMint, OutputMint: pair.OutputMint,
		InAmount: pair.InAmount, OutAmount: order.OutAmount, MinOutAmount: order.MinOut,
		RequestedMinOutAmount: request.MinOutAmount, SlippageBPS: request.SlippageBPS,
		SwapFeeBPS: order.FeeBPS, SwapFeeMint: order.FeeMint,
		SignatureFeeLamports: strconv.FormatUint(*order.SignatureFeeLamports, 10),
		PriorityFeeLamports:  strconv.FormatUint(*order.PriorityFeeLamports, 10),
		RentFeeLamports:      strconv.FormatUint(*order.RentFeeLamports, 10),
		Router:               order.Router, ProviderRequestID: order.RequestID, TransactionVersion: *order.TransactionVersion,
		TransactionBase64: *order.Transaction, LastValidBlockHeight: order.LastValidBlockHeight,
		RequestedAt: requestedAt, ObservedAt: observedAt, ProviderExpiresAt: order.ExpiresAt,
	}, nil
}

func (order preparedOrder) validFees(owner string) bool {
	return order.SignatureFeePayer != nil && *order.SignatureFeePayer == owner &&
		order.SignatureFeeLamports != nil && *order.SignatureFeeLamports > 0 &&
		coveredFee(order.PriorityFeeLamports, order.PriorityFeePayer, owner) &&
		coveredFee(order.RentFeeLamports, order.RentFeePayer, owner)
}

func coveredFee(amount *uint64, payer *string, owner string) bool {
	return amount != nil && ((payer == nil && *amount == 0) || (payer != nil && *payer == owner))
}

func validTransactionEncoding(encoded string) bool {
	if len(encoded) == 0 || len(encoded) > base64.StdEncoding.EncodedLen(maxV0TransactionBytes) ||
		strings.ContainsAny(encoded, "\r\n") {
		return false
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(encoded)
	return err == nil && len(raw) > 0 && len(raw) <= maxV0TransactionBytes
}
