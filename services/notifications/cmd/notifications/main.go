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
	analyticsURL := strings.TrimSpace(os.Getenv("TRADINGBOT_ANALYTICS_URL"))
	if analyticsURL != "" {
		owner := strings.TrimSpace(os.Getenv("TRADINGBOT_TELEGRAM_OWNER_USER_ID"))
		commands, err := notifications.NewPortfolioCommands(telegram, analyticsURL, owner)
		if err != nil {
			return err
		}
		workerContext, stop := context.WithCancel(ctx)
		defer stop()
		done := make(chan struct{})
		go func() {
			defer close(done)
			commands.Run(workerContext)
		}()
		err = notifications.Consume(workerContext, kafkaConfig.Brokers, kafkaConfig.Topic, group, telegram.Send)
		stop()
		<-done
		return err
	}
	return notifications.Consume(ctx, kafkaConfig.Brokers, kafkaConfig.Topic, group, telegram.Send)
}
