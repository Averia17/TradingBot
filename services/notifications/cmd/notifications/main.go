package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"tradingbot/notifications/internal/kafka"
	"tradingbot/notifications/internal/notifications"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("notifications stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	telegram, err := notifications.NewTelegram(os.Getenv("TRADINGBOT_TELEGRAM_BOT_TOKEN"), os.Getenv("TRADINGBOT_TELEGRAM_CHAT_ID"))
	if err != nil {
		return err
	}
	kafkaConfig, err := kafka.ReadConfig()
	if err != nil {
		return err
	}
	group := strings.TrimSpace(os.Getenv("TRADINGBOT_NOTIFICATION_GROUP"))
	if group == "" {
		group = "tradingbot-telegram-v1"
	}
	return notifications.Consume(ctx, kafkaConfig.Brokers, kafkaConfig.Topic, group, telegram.Send)
}
