package notifications

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"

	"tradingbot/notifications/internal/fill_event"
)

// TestJupiterPaperFillsReachTelegram consumes the exact outbox of the
// scheduled or provider-backed paper rehearsal through the isolated HTTP outbox.
func TestJupiterPaperFillsReachTelegram(t *testing.T) {
	dsn, raw := os.Getenv("TRADINGBOT_TEST_JUPITER_NOTIFICATION_DSN"), os.Getenv("TRADINGBOT_TEST_JUPITER_NOTIFICATION_EVENTS")
	source, token := os.Getenv("TRADINGBOT_TEST_JUPITER_NOTIFICATION_ACCOUNTS_URL"), os.Getenv("TRADINGBOT_TEST_JUPITER_NOTIFICATION_TOKEN")
	if dsn == "" && raw == "" && source == "" && token == "" {
		t.Skip("run through an opt-in scheduled or provider-backed Control rehearsal")
	}
	if !validNotificationDatabase(dsn) {
		t.Fatal("notification rehearsal requires an isolated loopback paper rehearsal schema")
	}
	accountsURL, err := url.Parse(source)
	if err != nil || accountsURL.Scheme != "http" || !numericNotificationLoopback(accountsURL.Host) ||
		accountsURL.User != nil || accountsURL.Path != "" || accountsURL.RawQuery != "" || accountsURL.Fragment != "" || token == "" {
		t.Fatal("notification rehearsal requires an authenticated loopback Accounts HTTP endpoint")
	}
	var payloads []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &payloads); err != nil || len(payloads) != 2 {
		t.Fatal("notification rehearsal requires the exact BUY and SELL ledger fills")
	}
	expected := make(map[string]fill_event.PaperEvent, 2)
	texts := make(map[string]string, 2)
	sides := [2]string{"BUY", "SELL"}
	for i, payload := range payloads {
		event, err := fill_event.DecodePaper(payload)
		if err != nil || event.Side != sides[i] || event.ResearchJobID == "" {
			t.Fatalf("invalid committed paper fill for leg %d", i+1)
		}
		expected[event.ID] = event
		texts[paperMessage(event)] = event.ID
	}
	if len(expected) != 2 || len(texts) != 2 {
		t.Fatal("paper fills did not produce distinct identities")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 70*time.Second)
	defer cancel()
	const topic = fill_event.PaperTopic
	brokers := notificationBrokers(t, ctx)
	group := "jupiter-paper-notifications-" + strings.ToLower(rand.Text())
	observer, err := kgo.NewClient(kgo.SeedBrokers(brokers...), kgo.ConsumePartitions(map[string]map[int32]kgo.Offset{
		topic: {0: kgo.NewOffset().AtStart()},
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	startPaperNotificationRelay(t, ctx, dsn, source, token, brokers)

	deliveries := make(chan string, 3)
	telegram := testTelegram(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var message struct {
			ChatID int64  `json:"chat_id"`
			Text   string `json:"text"`
		}
		err := json.NewDecoder(io.LimitReader(r.Body, 32<<10)).Decode(&message)
		id, found := texts[message.Text]
		event := expected[id]
		marker := "[PAPER · СИМУЛЯЦИЯ] Покупка "
		if event.Side == "SELL" {
			marker = "[PAPER · СИМУЛЯЦИЯ] Продажа "
		}
		if r.Method != http.MethodPost || r.URL.Path != "/bot"+fakeToken+"/sendMessage" ||
			err != nil || message.ChatID != 4294967297 || !found ||
			!strings.HasPrefix(message.Text, marker+event.InstrumentID+"\n") ||
			!strings.Contains(message.Text, "\nВаловая сумма: "+event.GrossUSD+" USD\n") {
			t.Error("Telegram request differs from the committed paper fill")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"ok":false,"error_code":400}`)
			return
		}
		select {
		case deliveries <- id:
			writeTelegramSuccess(w)
		default:
			t.Error("too many Telegram delivery attempts")
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	consumerCtx, stop := context.WithCancel(ctx)
	var consumerErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		consumerErr = Consume(consumerCtx, brokers, topic, group, telegram.Send)
	}()
	defer func() {
		stop()
		<-done
		if consumerErr != nil {
			t.Error(consumerErr)
		}
	}()

	for seen := make(map[string]bool, 2); len(seen) < len(expected); {
		fetch := observer.PollRecords(ctx, len(expected)-len(seen))
		if err := fetch.Err(); err != nil {
			t.Fatal(err)
		}
		for _, record := range fetch.Records() {
			event, err := fill_event.DecodePaper(record.Value)
			if err != nil || seen[event.ID] || string(record.Key) != event.ID || expected[event.ID] != event {
				t.Fatalf("Kafka record does not match an exact booked paper fill: partition=%d offset=%d", record.Partition, record.Offset)
			}
			seen[event.ID] = true
		}
	}
	for delivered := make(map[string]bool, 2); len(delivered) < len(expected); {
		select {
		case id := <-deliveries:
			if delivered[id] {
				t.Fatal("duplicate Telegram delivery in the controlled rehearsal")
			}
			delivered[id] = true
		case <-done:
			t.Fatalf("notification consumer stopped early: %v", consumerErr)
		case <-ctx.Done():
			t.Fatal("Telegram fixture did not confirm both paper messages")
		}
	}
	waitForPaperNotificationCommit(t, ctx, observer, group)
	waitForAccountAcknowledgements(t, ctx, source, token)
	end := kmsg.NewPtrListOffsetsRequest()
	end.Topics = []kmsg.ListOffsetsRequestTopic{{
		Topic: topic, Partitions: []kmsg.ListOffsetsRequestTopicPartition{{Partition: 0, Timestamp: -1, CurrentLeaderEpoch: -1}},
	}}
	tail, err := end.RequestWith(ctx, observer)
	if err != nil || len(tail.Topics) != 1 || len(tail.Topics[0].Partitions) != 1 ||
		tail.Topics[0].Partitions[0].ErrorCode != 0 || tail.Topics[0].Partitions[0].Offset != 2 || len(deliveries) != 0 {
		t.Fatal("Kafka/Telegram rehearsal produced an unexpected number of messages")
	}
	t.Log("the two exact committed ledger fills reached the paper Kafka topic and Telegram stub; consumer committed offset 2")
}

var paperNotificationSchema = regexp.MustCompile(`^trade_delivery_[a-z0-9]+$`)

func validNotificationDatabase(dsn string) bool {
	uri, err := url.Parse(dsn)
	if err != nil || (uri.Scheme != "postgresql" && uri.Scheme != "postgres") ||
		!numericNotificationLoopback(uri.Host) || uri.User == nil || uri.User.Username() == "" ||
		uri.Opaque != "" || uri.Fragment != "" || strings.Count(uri.Path, "/") != 1 || len(uri.Path) < 2 {
		return false
	}
	params, err := url.ParseQuery(uri.RawQuery)
	return err == nil && len(params) == 2 && len(params["sslmode"]) == 1 && params.Get("sslmode") == "disable" &&
		len(params["search_path"]) == 1 && paperNotificationSchema.MatchString(params.Get("search_path"))
}

func numericNotificationLoopback(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil || (host != "127.0.0.1" && host != "::1") {
		return false
	}
	number, err := strconv.ParseUint(port, 10, 16)
	return err == nil && number > 0
}

func notificationBrokers(t *testing.T, ctx context.Context) []string {
	t.Helper()
	configured := os.Getenv("TRADINGBOT_TEST_KAFKA_BROKERS")
	if configured == "" {
		cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, fill_event.PaperTopic))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(cluster.Close)
		t.Log("using an isolated Kafka protocol test broker (kfake)")
		return cluster.ListenAddrs()
	}
	brokers := strings.Split(configured, ",")
	for _, address := range brokers {
		if !numericNotificationLoopback(address) {
			t.Fatal("notification rehearsal Kafka brokers must use explicit loopback addresses")
		}
	}
	client, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	create := kmsg.NewPtrCreateTopicsRequest()
	create.Topics = []kmsg.CreateTopicsRequestTopic{
		{Topic: fill_event.PaperTopic, NumPartitions: 1, ReplicationFactor: 1},
	}
	result, err := create.RequestWith(ctx, client)
	if err != nil || len(result.Topics) != 1 || result.Topics[0].ErrorCode != 0 {
		t.Fatal("notification rehearsal requires a fresh local Kafka broker without the paper topic")
	}
	t.Log("using a local Kafka broker")
	return brokers
}

func startPaperNotificationRelay(t *testing.T, ctx context.Context, dsn, source, token string, brokers []string) {
	t.Helper()
	name := "paper-notification-relay"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(t.TempDir(), name)
	build := exec.CommandContext(ctx, "go", "build", "-mod=readonly", "-o", binary, "./cmd/trade-events")
	build.Dir = filepath.Join("..", "..", "..", "trade-events")
	build.Env = append(os.Environ(), "GOWORK=off")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build paper outbox relay: %v\n%s", err, output)
	}
	relay := exec.CommandContext(ctx, binary, "-paper-ledger")
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		name = strings.ToUpper(name)
		if !strings.HasPrefix(name, "TRADINGBOT_") && !strings.HasPrefix(name, "PG") {
			relay.Env = append(relay.Env, entry)
		}
	}
	relay.Env = append(relay.Env, "TRADINGBOT_DATABASE_URL="+dsn,
		"TRADINGBOT_ACCOUNTS_URL="+source, "TRADINGBOT_ACCOUNTS_OUTBOX_TOKEN="+token,
		"TRADINGBOT_KAFKA_BROKERS="+strings.Join(brokers, ","), "TRADINGBOT_TRADE_TOPIC="+fill_event.PaperTopic)
	relay.Stdout, relay.Stderr = os.Stdout, os.Stderr
	if err := relay.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = relay.Process.Kill()
		_ = relay.Wait()
	})
}

func waitForAccountAcknowledgements(t *testing.T, ctx context.Context, origin, token string) {
	t.Helper()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/internal/v1/paper/outbox/fills/next", nil)
		if err != nil {
			t.Fatal("create Accounts acknowledgement probe")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal("probe Accounts acknowledgement")
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNoContent {
			return
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("Accounts acknowledgement probe returned HTTP %d", resp.StatusCode)
		}
		select {
		case <-ctx.Done():
			t.Fatal("relay did not acknowledge the Accounts outbox")
		case <-ticker.C:
		}
	}
}

func waitForPaperNotificationCommit(t *testing.T, ctx context.Context, client *kgo.Client, group string) {
	t.Helper()
	request := kmsg.NewPtrOffsetFetchRequest()
	request.Group = group
	request.Groups = []kmsg.OffsetFetchRequestGroup{{Group: group, MemberEpoch: -1}}
	ticker := time.NewTicker(30 * time.Millisecond)
	defer ticker.Stop()
	for {
		response, err := request.RequestWith(ctx, client)
		if err == nil && response.ErrorCode == 0 {
			if len(response.Topics) == 1 && response.Topics[0].Topic == fill_event.PaperTopic &&
				len(response.Topics[0].Partitions) == 1 && response.Topics[0].Partitions[0].Offset == 2 {
				return
			}
			if len(response.Groups) == 1 && response.Groups[0].Group == group && len(response.Groups[0].Topics) == 1 &&
				len(response.Groups[0].Topics[0].Partitions) == 1 && response.Groups[0].Topics[0].Partitions[0].Offset == 2 {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("Kafka did not commit the two acknowledged paper notifications")
		case <-ticker.C:
		}
	}
}
