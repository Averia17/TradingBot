package notifications

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	defaultPortfolioPeriod = "30d"
	maxCachedReports       = 8
	maxUpdateResponse      = 512 << 10
	commandBatchSize       = 10
)

type PortfolioCommands struct {
	telegram  *Telegram
	analytics *portfolioClient
	ownerID   int64
	cache     [maxCachedReports]cachedPortfolio
	nextCache int
}

type cachedPortfolio struct {
	key    string
	report portfolioReport
}

func NewPortfolioCommands(telegram *Telegram, analyticsURL, owner string) (*PortfolioCommands, error) {
	analytics, err := newPortfolioClient(analyticsURL)
	if err != nil {
		return nil, err
	}
	id, err := strconv.ParseInt(owner, 10, 64)
	if err != nil || id <= 0 {
		return nil, errors.New("TRADINGBOT_TELEGRAM_OWNER_USER_ID must contain a positive numeric user ID")
	}
	if telegram == nil {
		return nil, errors.New("Telegram is required for portfolio commands")
	}
	return &PortfolioCommands{telegram: telegram, analytics: analytics, ownerID: id}, nil
}

type analyticsTelegramUser struct {
	ID    int64 `json:"id"`
	IsBot bool  `json:"is_bot"`
}

type analyticsTelegramChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type analyticsTelegramUpdate struct {
	ID      int64 `json:"update_id"`
	Message *struct {
		From analyticsTelegramUser `json:"from"`
		Chat analyticsTelegramChat `json:"chat"`
		Text string                `json:"text"`
	} `json:"message"`
	Callback *struct {
		ID      string                `json:"id"`
		From    analyticsTelegramUser `json:"from"`
		Message *struct {
			Chat analyticsTelegramChat `json:"chat"`
		} `json:"message"`
		Data string `json:"data"`
	} `json:"callback_query"`
}

// Run holds one long poll for the bot until shutdown. Telegram confirms the
// previous batch when the next request supplies its offset.
func (c *PortfolioCommands) Run(ctx context.Context) {
	slog.Info("polling Telegram for paper portfolio commands")
	var offset int64
	backoff := 2 * time.Second
	for ctx.Err() == nil {
		if err := c.poll(ctx, &offset); err == nil {
			backoff = 2 * time.Second
		} else if ctx.Err() == nil {
			slog.Warn("Telegram portfolio command poll failed", "error", err)
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
			backoff = min(backoff*2, 30*time.Second)
		}
	}
}

func (c *PortfolioCommands) poll(ctx context.Context, offset *int64) error {
	request := struct {
		Offset  int64    `json:"offset"`
		Timeout int      `json:"timeout"`
		Limit   int      `json:"limit"`
		Types   []string `json:"allowed_updates"`
	}{*offset, 5, commandBatchSize, []string{"message", "callback_query"}}
	var updates []analyticsTelegramUpdate
	if err := c.telegram.commandRequest(ctx, "getUpdates", request, maxUpdateResponse, &updates); err != nil {
		return err
	}
	if updates == nil || len(updates) > commandBatchSize {
		return errors.New("Telegram update batch was invalid")
	}
	if len(updates) == 0 {
		// The previous offset is now confirmed. After a week without new
		// updates Telegram can choose a fresh random identifier.
		*offset = 0
		return nil
	}
	for _, update := range updates {
		if update.ID <= 0 || update.ID == math.MaxInt64 {
			return errors.New("Telegram delivered an invalid update identifier")
		}
	}
	slices.SortFunc(updates, func(a, b analyticsTelegramUpdate) int { return cmp.Compare(a.ID, b.ID) })
	for _, update := range updates {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if update.ID < *offset {
			continue
		}
		if err := c.handleUpdate(ctx, update); err != nil && ctx.Err() == nil {
			// A response may already have reached Telegram. Avoid a blind
			// resend on an ambiguous result.
			slog.Warn("Telegram portfolio command response failed", "update_id", update.ID, "error", err)
		}
		*offset = update.ID + 1
	}
	return nil
}

func (c *PortfolioCommands) handleUpdate(ctx context.Context, update analyticsTelegramUpdate) error {
	if m := update.Message; m != nil && c.isOwner(m.From, m.Chat) {
		return c.handleCommand(ctx, m.Text)
	}
	if q := update.Callback; q != nil && q.Message != nil && c.isOwner(q.From, q.Message.Chat) {
		return c.handleCallback(ctx, q.ID, q.Data)
	}
	return nil
}

func (c *PortfolioCommands) isOwner(user analyticsTelegramUser, chat analyticsTelegramChat) bool {
	return !user.IsBot && user.ID == c.ownerID && chat.ID == c.telegram.chatID && chat.Type == "private"
}

func (c *PortfolioCommands) handleCommand(ctx context.Context, message string) error {
	args := strings.Fields(message)
	if len(args) == 0 {
		return nil
	}
	maxArgs := 1
	switch args[0] {
	case "/portfolio", "/positions":
	case "/performance":
		maxArgs = 2
	default:
		return nil
	}
	period := defaultPortfolioPeriod
	if len(args) == 2 {
		period = args[1]
	}
	if len(args) > maxArgs || (period != "7d" && period != "30d" && period != "all") {
		return c.telegram.Send(ctx, "Команды: /portfolio, /positions, /performance [7d|30d|all].")
	}
	report, err := c.analytics.fetch(ctx, period)
	if err != nil {
		slog.Warn("paper portfolio report unavailable", "error", err)
		return c.telegram.Send(ctx, "Отчёт PAPER сейчас недоступен.")
	}
	sum := sha256.Sum256([]byte(report.ID))
	key := base64.RawURLEncoding.EncodeToString(sum[:12])
	c.cache[c.nextCache] = cachedPortfolio{key: key, report: report}
	c.nextCache = (c.nextCache + 1) % maxCachedReports
	return c.sendView(ctx, args[0][1:], 0, key, report)
}

func (c *PortfolioCommands) handleCallback(ctx context.Context, id, data string) error {
	if id == "" {
		return nil
	}
	parts := strings.Split(data, ":")
	if len(parts) != 4 || parts[0] != "report" {
		return nil
	}
	page, err := strconv.Atoi(parts[3])
	if err != nil || page < 0 {
		return nil
	}
	for _, entry := range c.cache {
		if entry.key == "" || entry.key != parts[1] || !entry.report.hasPage(parts[2], page) {
			continue
		}
		// A failed acknowledgement does not prevent the report reply.
		if err := c.answerCallback(ctx, id, ""); err != nil && ctx.Err() == nil {
			slog.Warn("Telegram portfolio button acknowledgement failed", "error", err)
		}
		return c.sendView(ctx, parts[2], page, entry.key, entry.report)
	}
	return c.answerCallback(ctx, id, "Отчёт устарел. Откройте /portfolio.")
}

func (c *PortfolioCommands) answerCallback(ctx context.Context, id, text string) error {
	request := struct {
		ID   string `json:"callback_query_id"`
		Text string `json:"text,omitempty"`
	}{id, text}
	var confirmed bool
	if err := c.telegram.commandRequest(ctx, "answerCallbackQuery", request, maxTelegramResponse, &confirmed); err != nil {
		return err
	}
	if !confirmed {
		return errors.New("Telegram did not confirm the button acknowledgement")
	}
	return nil
}

func (c *PortfolioCommands) sendView(ctx context.Context, view string, page int, key string, report portfolioReport) error {
	button := func(text, target string, targetPage int) telegramButton {
		return telegramButton{text, fmt.Sprintf("report:%s:%s:%d", key, target, targetPage)}
	}
	var content string
	var rows [][]telegramButton
	switch view {
	case "portfolio":
		content = portfolioSummary(report)
		rows = [][]telegramButton{{button("Позиции", "positions", 0), button("Доходность", "performance", 0)}}
	case "performance":
		content = portfolioPerformance(report)
		rows = [][]telegramButton{{button("Сводка", "portfolio", 0)}}
	case "positions":
		content = positionPage(report, page)
		var nav []telegramButton
		if page > 0 {
			nav = append(nav, button("Назад", "positions", page-1))
		}
		if page+1 < report.positionPageCount() {
			nav = append(nav, button("Дальше", "positions", page+1))
		}
		if len(nav) > 0 {
			rows = append(rows, nav)
		}
		rows = append(rows, []telegramButton{button("Сводка", "portfolio", 0)})
	}
	return c.telegram.sendMessage(ctx, content, &telegramKeyboard{Rows: rows})
}
