package outbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	paperOutboxPath     = "/internal/v1/paper/outbox"
	paperOutboxPageSize = 128
	maxPaperSourceBody  = 3 << 20
)

var errPaperSourceUnavailable = errors.New("accounts outbox temporarily unavailable")

type PaperSource struct {
	origin string
	token  string
	client *http.Client
}

type paperOutboxEntry struct {
	AccountID      string          `json:"account_id"`
	EventID        string          `json:"event_id"`
	AccountVersion int64           `json:"account_version,omitempty"`
	Payload        json.RawMessage `json:"payload"`
}

func NewPaperSource(origin, token string) (*PaperSource, error) {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Opaque != "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" ||
		(u.Path != "" && u.Path != "/") || strings.Contains(origin, "#") {
		return nil, errors.New("TRADINGBOT_ACCOUNTS_URL must be an HTTP origin")
	}
	if token == "" || strings.TrimSpace(token) != token || strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("an Accounts outbox token is required")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &PaperSource{
		origin: strings.TrimSuffix(u.String(), "/"),
		token:  token,
		client: &http.Client{
			Timeout:   8 * time.Second,
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (s *PaperSource) Close() { s.client.CloseIdleConnections() }

func (s *PaperSource) next(ctx context.Context, stream string) (*paperOutboxEntry, error) {
	var entry paperOutboxEntry
	status, err := s.call(ctx, http.MethodGet, "/"+stream+"/next", &entry)
	if err != nil || status == http.StatusNoContent {
		return nil, err
	}
	return &entry, nil
}

func (s *PaperSource) ack(ctx context.Context, path string) error {
	_, err := s.call(ctx, http.MethodPost, path+"/ack", nil)
	return err
}

func (s *PaperSource) accountHistory(ctx context.Context, accountID string, after, through int64) ([]paperOutboxEntry, error) {
	var entries []paperOutboxEntry
	path := "/accounts/" + accountID + "?after=" + strconv.FormatInt(after, 10) + "&through=" + strconv.FormatInt(through, 10)
	_, err := s.call(ctx, http.MethodGet, path, &entries)
	return entries, err
}

func (s *PaperSource) heads(ctx context.Context, after string) ([]paperAccountHead, error) {
	var heads []paperAccountHead
	_, err := s.call(ctx, http.MethodGet, "/heads?after="+after, &heads)
	return heads, err
}

func (s *PaperSource) head(ctx context.Context, accountID string) (paperAccountHead, error) {
	var head paperAccountHead
	_, err := s.call(ctx, http.MethodGet, "/heads/"+accountID, &head)
	return head, err
}

func (s *PaperSource) call(ctx context.Context, method, path string, value any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.origin+paperOutboxPath+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, fmt.Errorf("%w: %v", errPaperSourceUnavailable, err)
	}
	defer resp.Body.Close()
	status := resp.StatusCode
	if status == http.StatusNoContent && (method == http.MethodPost || strings.HasSuffix(path, "/next")) {
		return status, nil
	}
	if status >= 500 || status == http.StatusTooManyRequests || status == http.StatusRequestTimeout {
		return status, fmt.Errorf("%w: HTTP %d", errPaperSourceUnavailable, status)
	}
	if status != http.StatusOK || value == nil {
		return status, fmt.Errorf("invalid accounts outbox response: HTTP %d", status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPaperSourceBody+1))
	if err != nil {
		return status, fmt.Errorf("%w: read response", errPaperSourceUnavailable)
	}
	if len(data) == 0 || len(data) > maxPaperSourceBody {
		return status, errors.New("invalid accounts outbox response size")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil || decoder.Decode(new(any)) != io.EOF {
		return status, errors.New("invalid accounts outbox response JSON")
	}
	return status, nil
}
