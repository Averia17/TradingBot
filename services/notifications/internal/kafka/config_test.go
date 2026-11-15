package kafka

import (
	"slices"
	"strings"
	"testing"

	"tradingbot/notifications/internal/fill_event"
)

func TestReadConfigUsesDefaultTopic(t *testing.T) {
	t.Setenv("TRADINGBOT_KAFKA_BROKERS", " localhost:9092, [::1]:19092 ")
	t.Setenv("TRADINGBOT_TRADE_TOPIC", "")
	config, err := ReadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(config.Brokers, []string{"localhost:9092", "[::1]:19092"}) || config.Topic != fill_event.DefaultTopic {
		t.Fatalf("unexpected shared config: %+v", config)
	}
}

func TestReadConfigRejectsInvalidKafkaSettings(t *testing.T) {
	for _, brokers := range []string{"", "localhost", "broker:0", "broker:65536", "broker:9092,", "kafka://user:token@broker:9092"} {
		t.Setenv("TRADINGBOT_KAFKA_BROKERS", brokers)
		t.Setenv("TRADINGBOT_TRADE_TOPIC", fill_event.DefaultTopic)
		if _, err := ReadConfig(); err == nil || strings.Contains(err.Error(), "user:token") {
			t.Fatal("expected sanitized broker config error")
		}
	}
	for _, topic := range []string{".", "..", "trade.*", "two words", strings.Repeat("a", 250)} {
		t.Setenv("TRADINGBOT_KAFKA_BROKERS", "localhost:9092")
		t.Setenv("TRADINGBOT_TRADE_TOPIC", topic)
		if _, err := ReadConfig(); err == nil {
			t.Fatalf("unexpectedly accepted topic %q", topic)
		}
	}
}
