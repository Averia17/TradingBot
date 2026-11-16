package researchscheduler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func testSchedule(t *testing.T, at, snapshot time.Time, archive []byte) string {
	t.Helper()
	return fmt.Sprintf(`{
	"schema_version":"0.1","schedule_id":"offline-paper","policy_version":"frozen-v1",
	"max_lateness_seconds":60,"occurrences":[{"scheduled_at":%q,
	"portfolio":{"snapshot_id":"00000000-0000-0000-0000-000000000098","as_of":%q,
	"currency":"USD","cash_asset":"USDC","cash_usd":"1000","reserved_cash_usd":"0","max_position_pct":"0.25","positions":[]},
	"requested_symbols":["TSLA"],"data_manifest":{"ref":"fixture.json","sha256":"%x"},"max_llm_calls":20}]}`, at.Format(time.RFC3339Nano), snapshot.Format(time.RFC3339Nano), sha256.Sum256(archive))
}

func TestScheduleRepeatUsesSameJobAndSkipsOutsideWindow(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	archive := []byte(`{"schema_version":"0.1","source_mode":"synthetic_fixture","records":[]}`)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "fixture.json"), archive, 0600); err != nil {
		t.Fatal(err)
	}
	var jobIDs []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID string `json:"job_id"`
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/research/jobs" || json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid submission")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		jobIDs = append(jobIDs, body.ID)
		if len(jobIDs) == 1 {
			w.WriteHeader(http.StatusCreated)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"job_id": body.ID})
	}))
	defer server.Close()
	for _, delta := range []time.Duration{-2 * time.Second, -2 * time.Second, 20 * time.Second, -90 * time.Second} {
		schedule, err := Load(strings.NewReader(testSchedule(t, now.Add(delta), now.Add(delta-time.Second), archive)))
		if err != nil {
			t.Fatal(err)
		}
		if err := Run(context.Background(), schedule, Config{ControlURL: server.URL, ArchiveRoot: root, Once: true}); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(jobIDs) != 2 || jobIDs[0] == "" || jobIDs[0] != jobIDs[1] {
		t.Fatalf("want two durable submissions with the same ID, got %v", jobIDs)
	}
}

func TestRejectLookaheadAndUnverifiedArchive(t *testing.T) {
	now := time.Now().UTC()
	archive := []byte("archive with a declared hash")
	text := testSchedule(t, now.Add(-time.Second), now, archive)
	if _, err := Load(strings.NewReader(text)); err == nil {
		t.Fatal("future portfolio at scheduled time was accepted")
	}
	schedule, err := Load(strings.NewReader(testSchedule(t, now.Add(-time.Second), now.Add(-time.Second), archive)))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "fixture.json"), []byte("different"), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("unverified archive produced a request")
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()
	if err := Run(context.Background(), schedule, Config{ControlURL: server.URL, ArchiveRoot: root, Once: true}); err == nil {
		t.Fatal("archive hash mismatch was accepted")
	}
}

func TestCancellationWhileWaitingForNextOccurrence(t *testing.T) {
	next := time.Now().UTC().Add(time.Minute)
	schedule, err := Load(strings.NewReader(testSchedule(t, next, next, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := Run(ctx, schedule, Config{ControlURL: "http://127.0.0.1:18080", ArchiveRoot: t.TempDir(), PollInterval: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancellation did not stop the scheduler")
	}
}

func TestPublishedScheduleContractAndFixture(t *testing.T) {
	const origin = "https://tradingbot.local/contracts/"
	root := filepath.Join("..", "..", "..", "..", "contracts")
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	for _, name := range []string{"portfolio.schema.json", "research-job.schema.json", "research-schedule.schema.json"} {
		file, err := os.Open(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		doc, err := jsonschema.UnmarshalJSON(file)
		file.Close()
		if err != nil {
			t.Fatal(err)
		}
		if err := c.AddResource(origin+name, doc); err != nil {
			t.Fatal(err)
		}
	}
	schema, err := c.Compile(origin + "research-schedule.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "valid-research-schedule.json"))
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := jsonschema.UnmarshalJSON(strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(fixture); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(strings.NewReader(string(data))); err != nil {
		t.Fatal(err)
	}
}
