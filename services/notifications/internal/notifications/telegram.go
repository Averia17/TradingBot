package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

const (
	telegramTimeout     = 8 * time.Second
	maxTelegramWait     = 10 * time.Second
	maxTelegramResponse = 32 << 10
)

var botTokenPattern = regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]+$`)

type Telegram struct {
	client   *http.Client
	endpoint string
	chatID   int64
}

func NewTelegram(token, chatID string) (*Telegram, error) {
	if !botTokenPattern.MatchString(token) {
		return nil, errors.New("TRADINGBOT_TELEGRAM_BOT_TOKEN must contain a BotFather token")
	}
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil || id <= 0 {
		return nil, errors.New("TRADINGBOT_TELEGRAM_CHAT_ID must contain a positive numeric private chat ID")
	}
	client := &http.Client{
		Timeout: telegramTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &Telegram{client: client, endpoint: "https://api.telegram.org/bot" + token + "/sendMessage", chatID: id}, nil
}

func (t *Telegram) Send(ctx context.Context, message string) error {
	payload, err := json.Marshal(struct {
		ChatID int64  `json:"chat_id"`
		Text   string `json:"text"`
	}{t.chatID, message})
	if err != nil {
		return fmt.Errorf("encode Telegram request: %w", err)
	}
	retryAfter, err := t.send(ctx, payload)
	if retryAfter == 0 {
		return err
	}
	timer := time.NewTimer(retryAfter)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		_, err := t.send(ctx, payload)
		return err
	}
}

func (t *Telegram) send(ctx context.Context, payload []byte) (time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(payload))
	if err != nil {
		return 0, errors.New("cannot build Telegram request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		// net/http errors include the URL, which contains the bot token.
		return 0, errors.New("Telegram request failed with unknown delivery status")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTelegramResponse+1))
	if err != nil || len(body) > maxTelegramResponse {
		return 0, fmt.Errorf("Telegram response could not be read (HTTP %d)", resp.StatusCode)
	}
	var result struct {
		OK         *bool `json:"ok"`
		ErrorCode  int   `json:"error_code"`
		Parameters struct {
			RetryAfterSeconds int `json:"retry_after"`
		} `json:"parameters"`
		Result struct {
			MessageID int64 `json:"message_id"`
			Chat      struct {
				ID   int64  `json:"id"`
				Type string `json:"type"`
			} `json:"chat"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &result); err != nil || result.OK == nil {
		return 0, fmt.Errorf("Telegram response was invalid (HTTP %d)", resp.StatusCode)
	}
	if !*result.OK || resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("Telegram did not confirm delivery (HTTP %d, API code %d)", resp.StatusCode, result.ErrorCode)
		seconds := result.Parameters.RetryAfterSeconds
		if !*result.OK && resp.StatusCode == http.StatusTooManyRequests && result.ErrorCode == http.StatusTooManyRequests && seconds > 0 && seconds <= int(maxTelegramWait/time.Second) {
			return time.Duration(seconds) * time.Second, err
		}
		return 0, err
	}
	if result.Result.MessageID <= 0 || result.Result.Chat.ID != t.chatID || result.Result.Chat.Type != "private" {
		return 0, errors.New("Telegram response did not confirm a message in the configured private chat")
	}
	return 0, nil
}
