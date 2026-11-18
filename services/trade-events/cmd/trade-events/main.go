package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"golang.org/x/sync/errgroup"

	"tradingbot/trade-events/internal/exchange_fixture"
	"tradingbot/trade-events/internal/fill_event"
	"tradingbot/trade-events/internal/kafka"
	"tradingbot/trade-events/internal/outbox"
)

func main() {
	requeue := flag.Int64("requeue-task", 0, "requeue a failed trade outbox task by ID and exit")
	paperLedger := flag.Bool("paper-ledger", false, "relay authoritative virtual paper fills from Control's PostgreSQL database")
	paperAccount := flag.Bool("paper-account", false, "relay authoritative paper account events and publish account heads")
	replayAccount := flag.String("replay-paper-account", "", "republish the retained source events of one paper account and exit")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *requeue, *paperLedger, *paperAccount, *replayAccount); err != nil && ctx.Err() == nil {
		slog.Error("trade events stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, requeue int64, paperLedger, paperAccount bool, replayAccount string) error {
	dsn := os.Getenv("TRADINGBOT_DATABASE_URL")
	if dsn == "" {
		return errors.New("TRADINGBOT_DATABASE_URL is required")
	}
	accountMode := paperAccount || replayAccount != ""
	if accountMode && (paperLedger || requeue != 0 || paperAccount && replayAccount != "") {
		return errors.New("trade-events relay and maintenance modes are mutually exclusive")
	}
	if requeue != 0 {
		if requeue < 0 {
			return errors.New("-requeue-task must be a positive task ID")
		}
		return requeueTask(ctx, dsn, requeue)
	}
	kafkaConfig, err := kafka.ReadConfig()
	if err != nil {
		return err
	}
	address := os.Getenv("TRADINGBOT_TEST_EXCHANGE_ADDR")
	if paperLedger && (address != "" || kafkaConfig.Topic != fill_event.PaperTopic) {
		return errors.New("-paper-ledger requires the dedicated paper topic and an unset TRADINGBOT_TEST_EXCHANGE_ADDR")
	}
	if !paperLedger && kafkaConfig.Topic == fill_event.PaperTopic {
		return errors.New("dedicated paper topic requires -paper-ledger")
	}
	if accountMode && (address != "" || kafkaConfig.Topic != outbox.PaperAccountTopic) {
		return errors.New("paper account relay and replay require the dedicated portfolio account topic and an unset TRADINGBOT_TEST_EXCHANGE_ADDR")
	}
	if !accountMode && (kafkaConfig.Topic == outbox.PaperAccountTopic || kafkaConfig.Topic == outbox.PaperAccountHeadTopic) {
		return errors.New("dedicated portfolio account topics require -paper-account or -replay-paper-account")
	}
	headInterval := 20 * time.Second
	if value := os.Getenv("TRADINGBOT_PAPER_ACCOUNT_HEAD_INTERVAL"); paperAccount && value != "" {
		headInterval, err = time.ParseDuration(value)
		if err != nil || headInterval <= 0 {
			return errors.New("TRADINGBOT_PAPER_ACCOUNT_HEAD_INTERVAL must contain a positive duration")
		}
	}
	producer, err := kgo.NewClient(kgo.SeedBrokers(kafkaConfig.Brokers...), kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.RecordDeliveryTimeout(10*time.Second))
	if err != nil {
		return errors.New("invalid Kafka producer configuration")
	}
	defer producer.Close()
	startup, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	queue, err := outbox.Open(startup, dsn)
	if err != nil {
		return err
	}
	defer queue.Close()
	if err := producer.Ping(startup); err != nil {
		return errors.New("Kafka unreachable at startup")
	}
	if replayAccount != "" {
		count, err := queue.ReplayPaperAccount(ctx, producer, replayAccount)
		if err != nil {
			return err
		}
		slog.Info("paper account replay published", "account_id", replayAccount, "events", count)
		return nil
	}
	slog.Info("trade relay started", "topic", kafkaConfig.Topic)
	if paperAccount {
		return queue.RelayPaperAccounts(ctx, producer, headInterval)
	}
	if paperLedger {
		return queue.RelayPaper(ctx, producer)
	}
	if address == "" {
		return queue.Relay(ctx, producer, kafkaConfig.Topic)
	}
	if err := queue.PrepareExchangeFixtures(startup); err != nil {
		return err
	}
	listener, err := (&net.ListenConfig{}).Listen(startup, "tcp", address)
	if err != nil {
		return fmt.Errorf("listen for test exchange fills: %w", err)
	}
	defer listener.Close()
	cancel()
	return serveFixture(ctx, listener, queue, producer, kafkaConfig.Topic)
}

func serveFixture(ctx context.Context, listener net.Listener, queue *outbox.Outbox, producer *kgo.Client, topic string) error {
	server := &http.Server{
		Handler:           exchange_fixture.NewHandler(queue.RecordExchangeFixture),
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       15 * time.Second,
		MaxHeaderBytes:    8 * 1024,
	}
	group, runCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		err := queue.Relay(runCtx, producer, topic)
		if runCtx.Err() != nil {
			return nil
		}
		if err == nil {
			return errors.New("trade relay stopped unexpectedly")
		}
		return err
	})
	group.Go(func() error {
		slog.Info("test exchange listener started", "address", listener.Addr().String())
		if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("test exchange server: %w", err)
		}
		return nil
	})
	group.Go(func() error {
		<-runCtx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return fmt.Errorf("stop test exchange server: %w", err)
		}
		return nil
	})
	return group.Wait()
}

func requeueTask(ctx context.Context, dsn string, id int64) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	queue, err := outbox.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer queue.Close()
	if err := queue.Requeue(ctx, id); err != nil {
		return err
	}
	slog.Info("trade task requeued", "task_id", id)
	return nil
}
