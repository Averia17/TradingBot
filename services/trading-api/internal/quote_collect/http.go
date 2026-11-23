package quote_collect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"time"
	"unicode/utf8"

	"tradingbot/trading-api/internal/contract"
)

const maxFixtureResponseBytes = 16 * 1024

type fixtureRequest struct {
	Version    string `json:"paper_quote_fixture_schema_version"`
	InputMint  string `json:"input_mint"`
	OutputMint string `json:"output_mint"`
	InAmount   string `json:"in_amount"`
}

type fixtureResponse struct {
	fixtureRequest
	OutAmount    string    `json:"out_amount"`
	MinOutAmount string    `json:"min_out_amount"`
	ObservedAt   time.Time `json:"observed_at"`
	AvailableAt  time.Time `json:"available_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	FeeBPS       int       `json:"fee_bps"`
	FixedFeeUSD  string    `json:"fixed_fee_usd"`
}

type fixtureSource struct {
	endpoint string
	client   *http.Client
}

func openFixtureSource(endpoint string) (fixtureSource, error) {
	parsed, err := url.Parse(endpoint)
	invalidEndpoint := errors.New("quote fixture source must use an explicit loopback IP over HTTP, without credentials, query or fragment")
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.ForceQuery || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fixtureSource{}, invalidEndpoint
	}
	address, err := netip.ParseAddr(parsed.Hostname())
	if err != nil || !address.IsLoopback() || address.Zone() != "" {
		return fixtureSource{}, invalidEndpoint
	}
	return fixtureSource{
		endpoint: parsed.String(),
		client: &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				DisableCompression: true, MaxResponseHeaderBytes: maxFixtureResponseBytes,
			},
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func (source fixtureSource) fetch(ctx context.Context, request fixtureRequest) (fixtureResponse, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return fixtureResponse{}, err
	}
	message, err := http.NewRequestWithContext(ctx, http.MethodPost, source.endpoint, bytes.NewReader(payload))
	if err != nil {
		return fixtureResponse{}, errors.New("build local quote fixture request failed")
	}
	message.Header.Set("Content-Type", "application/json")
	message.Header.Set("Accept", "application/json")
	response, err := source.client.Do(message)
	if err != nil {
		if ctx.Err() != nil {
			return fixtureResponse{}, ctx.Err()
		}
		return fixtureResponse{}, errors.New("local quote fixture request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fixtureResponse{}, fmt.Errorf("local quote fixture returned HTTP %d", response.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(response.Body, maxFixtureResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return fixtureResponse{}, ctx.Err()
		}
		return fixtureResponse{}, errors.New("read local quote fixture response failed")
	}
	var reply fixtureResponse
	if len(raw) > maxFixtureResponseBytes || !utf8.Valid(raw) || contract.Decode(raw, contract.PaperQuoteFixtureResponse, &reply) != nil {
		return fixtureResponse{}, errors.New("invalid local quote fixture response")
	}
	return reply, nil
}
