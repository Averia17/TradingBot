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
	client *http.Client
	botURL string
	chatID int64
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
	return &Telegram{client: client, botURL: "https://api.telegram.org/bot" + token, chatID: id}, nil
}

func (t *Telegram) Send(ctx context.Context, message string) error {
	return t.sendMessage(ctx, message, nil)
}

type telegramButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

type telegramKeyboard struct {
	Rows [][]telegramButton `json:"inline_keyboard"`
}

func (t *Telegram) sendMessage(ctx context.Context, message string, keyboard *telegramKeyboard) error {
	payload, err := json.Marshal(struct {
		ChatID   int64             `json:"chat_id"`
		Text     string            `json:"text"`
		Keyboard *telegramKeyboard `json:"reply_markup,omitempty"`
	}{t.chatID, message, keyboard})
	if err != nil {
		return fmt.Errorf("encode Telegram request: %w", err)
	}
	retryAfter, err := t.trySendMessage(ctx, payload)
	if retryAfter == 0 {
		return err
	}
	timer := time.NewTimer(retryAfter)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		_, err := t.trySendMessage(ctx, payload)
		return err
	}
}

func (t *Telegram) trySendMessage(ctx context.Context, payload []byte) (time.Duration, error) {
	resp, err := t.post(ctx, "sendMessage", payload)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := readBoundedBody(resp.Body, maxTelegramResponse)
	if err != nil {
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

func (t *Telegram) commandRequest(ctx context.Context, method string, value any, limit int64, result any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return errors.New("cannot encode Telegram command request")
	}
	response, err := t.post(ctx, method, payload)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Telegram command request failed (HTTP %d)", response.StatusCode)
	}
	body, err := readBoundedBody(response.Body, limit)
	if err != nil {
		return errors.New("Telegram command response could not be read")
	}
	var reply struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(body, &reply) != nil || !reply.OK || json.Unmarshal(reply.Result, result) != nil {
		return errors.New("Telegram command request was unsuccessful")
	}
	return nil
}

func (t *Telegram) post(ctx context.Context, method string, payload []byte) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, t.botURL+"/"+method, bytes.NewReader(payload))
	if err != nil {
		return nil, errors.New("cannot build Telegram request")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := t.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// net/http errors include the URL, which contains the bot token.
		return nil, errors.New("Telegram request failed with unknown delivery status")
	}
	return response, nil
}

func readBoundedBody(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errors.New("HTTP response exceeds the size limit")
	}
	return body, nil
}
