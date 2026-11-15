package kafka

import (
	"errors"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"

	"tradingbot/notifications/internal/fill_event"
)

var topicPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,249}$`)

type Config struct {
	Brokers []string
	Topic   string
}

func ReadConfig() (Config, error) {
	brokers := strings.Split(os.Getenv("TRADINGBOT_KAFKA_BROKERS"), ",")
	for i, broker := range brokers {
		brokers[i] = strings.TrimSpace(broker)
		host, port, err := net.SplitHostPort(brokers[i])
		portNumber, portErr := strconv.ParseUint(port, 10, 16)
		if err != nil || host == "" || strings.ContainsAny(host, " \t\r\n/@") || portErr != nil || portNumber == 0 {
			return Config{}, errors.New("TRADINGBOT_KAFKA_BROKERS must contain comma-separated host:port addresses")
		}
	}

	topic := strings.TrimSpace(os.Getenv("TRADINGBOT_TRADE_TOPIC"))
	if topic == "" {
		topic = fill_event.DefaultTopic
	}
	if !topicPattern.MatchString(topic) || topic == "." || topic == ".." {
		return Config{}, errors.New("TRADINGBOT_TRADE_TOPIC must contain a valid Kafka topic name")
	}
	return Config{Brokers: brokers, Topic: topic}, nil
}
