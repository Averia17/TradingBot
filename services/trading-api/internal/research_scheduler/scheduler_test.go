package research_scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"tradingbot/trading-api/internal/contract"
)

const testControlURL = "http://127.0.0.1:18080"

type handlerTransport struct {
	http.Handler
}

func (transport handlerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Body != nil {
		defer request.Body.Close()
	}
	response := httptest.NewRecorder()
	transport.ServeHTTP(response, request)
	return response.Result(), nil
}

func runOnceWithHandler(schedule Schedule, root string, handler http.HandlerFunc) error {
	client := &http.Client{Transport: handlerTransport{handler}}
	config := Config{ControlURL: testControlURL, ArchiveRoot: root, Once: true}
	return runSchedule(context.Background(), schedule, config, client)
}

func testSchedule(t *testing.T, at, snapshot time.Time, archive []byte) string {
	t.Helper()
	return fmt.Sprintf(`{
	"schema_version":"0.1","schedule_id":"offline-paper","policy_version":"frozen-v1",
	"max_lateness_seconds":60,"occurrences":[{"scheduled_at":%q,
	"portfolio":{"snapshot_id":"00000000-0000-0000-0000-000000000098","as_of":%q,
	"currency":"USD","cash_asset":"USDC","cash_usd":"1000","reserved_cash_usd":"0","max_position_pct":"0.25","positions":[]},
	"requested_symbols":["TSLA"],"data_manifest":{"ref":"fixture.json","sha256":"%x"},"max_llm_calls":20}]}`, at.Format(time.RFC3339Nano), snapshot.Format(time.RFC3339Nano), sha256.Sum256(archive))
}

func eventSchedule(t *testing.T, kind, record string, at, snapshot time.Time, archive []byte) string {
	t.Helper()
	content := strings.Replace(testSchedule(t, at, snapshot, archive), `"schema_version":"0.1"`, `"schema_version":"0.2"`, 1)
	event := fmt.Sprintf(`"event":{"kind":%q,"provider":"fixture","record_id":%q},`, kind, record)
	return strings.Replace(content, `"portfolio":`, event+`"portfolio":`, 1)
}

func eventArchive(kind, payload, symbol string, at, available, ingested time.Time) []byte {
	return fmt.Appendf(nil, `{"schema_version":"0.1","source_mode":"synthetic_fixture","records":[{"kind":%q,"provider":"fixture","record_id":"catalyst","symbol":%s,"revision":1,"event_time":%q,"available_at":%q,"ingested_at":%q,"payload":%s}]}`,
		kind, symbol, at.Format(time.RFC3339Nano), available.Format(time.RFC3339Nano), ingested.Format(time.RFC3339Nano), payload)
}

func TestEventIdentitySurvivesDeliveryRescheduleAndInputChanges(t *testing.T) {
	now := time.Now().UTC()
	archive := eventArchive("filing", "{}", `"TSLA"`, now, now, now)
	first, err := Load(strings.NewReader(eventSchedule(t, "report", "catalyst", now, now, archive)))
	if err != nil {
		t.Fatal(err)
	}
	changes := eventArchive("filing", `{"reported_value":"2.50"}`, `"TSLA"`, now, now, now)
	restart, err := Load(strings.NewReader(eventSchedule(t, "report", "catalyst", now.Add(time.Second), now, changes)))
	if err != nil {
		t.Fatal(err)
	}
	if first.occurrences[0].ID != restart.occurrences[0].ID || string(first.occurrences[0].Body) == string(restart.occurrences[0].Body) {
		t.Fatal("reschedule and payload edits must preserve the deduplication key, not the immutable job request")
	}
}

func TestEventRequiresHashedPointInTimeEvidence(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	cutoff := now.Add(-5 * time.Second)
	known := cutoff.Add(-time.Second)
	for _, test := range []struct {
		name, trigger, kind, record, symbol, payload string
		ingested                                     time.Time
		valid                                        bool
	}{
		{"report", "report", "filing", "catalyst", `"TSLA"`, "{}", cutoff, true},
		{"whole-market news", "news", "news", "catalyst", `null`, "{}", cutoff, true},
		{"price", "price", "bar", "catalyst", `"TSLA"`, "{}", cutoff, true},
		{"liquidity", "liquidity", "macro", "catalyst", `"TSLA"`, `{"trigger_kind":"liquidity"}`, cutoff, true},
		{"policy", "policy", "macro", "catalyst", `null`, `{"trigger_kind":"policy"}`, cutoff, true},
		{"review", "review", "macro", "catalyst", `"TSLA"`, `{"trigger_kind":"review"}`, cutoff, true},
		{"invented report", "report", "filing", "nonexistent", `"TSLA"`, "{}", cutoff, false},
		{"wrong symbol", "report", "filing", "catalyst", `"NVDA"`, "{}", cutoff, false},
		{"unrelated macro", "policy", "macro", "catalyst", `null`, `{"trigger_kind":"review"}`, cutoff, false},
		{"late ingestion", "report", "filing", "catalyst", `"TSLA"`, "{}", cutoff.Add(time.Second), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			archive := eventArchive(test.kind, test.payload, test.symbol, known, known, test.ingested)
			schedule, err := Load(strings.NewReader(eventSchedule(t, test.trigger, test.record, now.Add(-time.Second), cutoff, archive)))
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "fixture.json"), archive, 0600); err != nil {
				t.Fatal(err)
			}
			err = runOnceWithHandler(schedule, root, func(w http.ResponseWriter, r *http.Request) {
				if !test.valid {
					t.Error("unproven event produced a research request")
				}
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]string{"job_id": schedule.occurrences[0].ID})
			})
			if (err == nil) != test.valid {
				t.Fatalf("event proof acceptance=%t, want %t (%v)", err == nil, test.valid, err)
			}
		})
	}
}

func TestEventUsesLatestAvailableRevisionAndIgnoresFutureRevision(t *testing.T) {
	cutoff := time.Now().UTC().Truncate(time.Second)
	known := cutoff.Add(-time.Second)
	var archive struct {
		Version string         `json:"schema_version"`
		Source  string         `json:"source_mode"`
		Records []archiveEvent `json:"records"`
	}
	if err := json.Unmarshal(eventArchive("macro", `{"trigger_kind":"policy"}`, `null`, known, known, known), &archive); err != nil {
		t.Fatal(err)
	}
	second := archive.Records[0]
	second.Revision = 2
	second.AvailableAt, second.IngestedAt = cutoff.Add(time.Second), cutoff.Add(time.Second)
	second.Payload = json.RawMessage(`{"trigger_kind":"review"}`)
	archive.Records = append(archive.Records, second)
	lookup := eventLookup{reference: EventReference{Kind: "policy", Provider: "fixture", RecordID: "catalyst"}, cutoff: cutoff, symbols: []string{"TSLA"}}
	data, err := json.Marshal(archive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lookup.verify(data, "fixture"); err != nil {
		t.Fatalf("future revision affected event selection: %v", err)
	}
	archive.Records[1].AvailableAt = cutoff
	archive.Records[1].Payload = json.RawMessage(`{"trigger_kind":"policy"}`)
	data, err = json.Marshal(archive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lookup.verify(data, "fixture"); err == nil {
		t.Fatal("known but not yet ingested event revision was accepted")
	}
}

func TestEventProofAcceptsArchiveWithPaperMemory(t *testing.T) {
	cutoff := time.Now().UTC().Truncate(time.Second)
	known := cutoff.Add(-time.Second)
	var archive struct {
		Version string         `json:"schema_version"`
		Source  string         `json:"source_mode"`
		Records []archiveEvent `json:"records"`
	}
	if err := json.Unmarshal(eventArchive("filing", "{}", `"TSLA"`, known, known, known), &archive); err != nil {
		t.Fatal(err)
	}
	memory := archive.Records[0]
	memory.Kind, memory.Provider, memory.RecordID = "paper_outcome", "paper-ledger", "previous-fill"
	archive.Version = "0.2"
	archive.Records = append(archive.Records, memory)
	lookup := eventLookup{reference: EventReference{Kind: "report", Provider: "fixture", RecordID: "catalyst"}, cutoff: cutoff, symbols: []string{"TSLA"}}
	data, err := json.Marshal(archive)
	if err != nil {
		t.Fatal(err)
	}
	if proof, err := lookup.verify(data, "fixture"); err != nil || proof.Kind() != "report" || !proof.KnownAt().Equal(known) {
		t.Fatalf("memory changed the catalyst proof: %+v / %v", proof, err)
	}
	lookup.reference.Provider, lookup.reference.RecordID = memory.Provider, memory.RecordID
	if _, err := lookup.verify(data, "fixture"); err == nil {
		t.Fatal("paper memory was accepted as a filing trigger")
	}
}

func TestRetryChecksCurrentArchiveDigest(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	archiveBytes := eventArchive("filing", "{}", `"TSLA"`, now, now, now)
	rawSchedule := eventSchedule(t, "report", "catalyst", now, now, archiveBytes)
	schedule, err := Load(strings.NewReader(rawSchedule))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	archivePath := filepath.Join(root, "fixture.json")
	if err := os.WriteFile(archivePath, archiveBytes, 0600); err != nil {
		t.Fatal(err)
	}
	archive, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	requests := 0
	transport := handlerTransport{http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if err := os.WriteFile(archivePath, append(archiveBytes, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})}
	control := controlClient{client: &http.Client{Transport: transport}, origin: testControlURL}
	job := schedule.occurrences[0]
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err = job.run(ctx, archive, control, Config{PollInterval: time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "SHA-256") || requests != 1 {
		t.Fatalf("expected a changed archive to stop retry, requests = %d and error = %v", requests, err)
	}
}

func TestScheduleRepeatUsesSameJobAndSkipsOutsideWindow(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	archive := []byte(`{"schema_version":"0.1","source_mode":"synthetic_fixture","records":[]}`)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "fixture.json"), archive, 0600); err != nil {
		t.Fatal(err)
	}
	var jobIDs []string
	handler := func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID string `json:"job_id"`
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/research/jobs" || json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid submission")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		jobIDs = append(jobIDs, body.ID)
		if len(jobIDs) == 1 {
			w.WriteHeader(http.StatusCreated)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"job_id": body.ID})
	}
	for _, delta := range []time.Duration{-2 * time.Second, -2 * time.Second, 20 * time.Second, -48 * time.Hour} {
		schedule, err := Load(strings.NewReader(testSchedule(t, now.Add(delta), now.Add(delta-time.Second), archive)))
		if err != nil {
			t.Fatal(err)
		}
		if err := runOnceWithHandler(schedule, root, handler); err != nil {
			t.Fatal(err)
		}
	}
	if len(jobIDs) != 2 || jobIDs[0] == "" || jobIDs[0] != jobIDs[1] {
		t.Fatalf("want two durable submissions with the same ID, got %v", jobIDs)
	}
}

func TestCalendarOccurrenceFixtureMatchesPublishedSchedule(t *testing.T) {
	const root = "../../../../contracts/"
	scheduleFile, err := os.Open(root + "valid-research-schedule.json")
	if err != nil {
		t.Fatal(err)
	}
	defer scheduleFile.Close()
	schedule, err := Load(scheduleFile)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(root + "valid-research-calendar-occurrence.json")
	if err != nil {
		t.Fatal(err)
	}
	var receipt contract.ResearchCalendarReceipt
	if err := contract.Decode(raw, contract.ResearchCalendarOccurrenceReceipt, &receipt); err != nil {
		t.Fatal(err)
	}
	calendar, excluded := schedule.PlannedCalendar()
	job := schedule.occurrences[0]
	if len(calendar) != 1 || excluded != 0 || receipt.JobID != job.ID ||
		receipt.ResearchCalendarOccurrence != calendar[0] || !receipt.Deadline().Equal(job.Deadline) ||
		receipt.RequestSHA256 != fmt.Sprintf("%x", sha256.Sum256(job.Body)) || receipt.RecordedAt.Before(receipt.Deadline()) {
		t.Fatalf("calendar occurrence fixture does not match the scheduled request: %+v", receipt)
	}
}

func TestMixedScheduleUsesFrozenCalendarEvidenceAndJobRequests(t *testing.T) {
	now := time.Now().UTC()
	at := now.Add(-5 * time.Second)
	closed := now.Add(-2 * time.Minute)
	future := now.Add(time.Minute)
	known := closed.Add(-time.Second)
	archive := eventArchive("filing", "{}", `"TSLA"`, known, known, known)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "fixture.json"), archive, 0600); err != nil {
		t.Fatal(err)
	}
	parse := func(value string) occurrenceSpec {
		t.Helper()
		var input scheduleSpec
		if err := json.Unmarshal([]byte(value), &input); err != nil {
			t.Fatal(err)
		}
		return input.Occurrences[0]
	}
	missed := parse(testSchedule(t, closed, known, archive))
	expiredEvent := parse(eventSchedule(t, "report", "expired", closed, known, archive))
	missed.Manifest.Ref, expiredEvent.Manifest.Ref = "absent.json", "absent.json"
	config := scheduleSpec{Version: "0.2", ID: "mixed", Policy: "frozen-v1", Lateness: 60, Occurrences: []occurrenceSpec{
		parse(testSchedule(t, future, future, archive)),
		expiredEvent,
		parse(eventSchedule(t, "report", "catalyst", at, at, archive)),
		missed,
		parse(testSchedule(t, at.Add(time.Second), at, archive)),
	}}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	schedule, err := Load(strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	calendar, excluded := schedule.PlannedCalendar()
	if len(calendar) != 3 || excluded != 2 || !calendar[0].ScheduledAt.Equal(closed) || !calendar[2].ScheduledAt.Equal(future) {
		t.Fatalf("unexpected coverage plan: %+v, excluded %d", calendar, excluded)
	}
	missedID := contract.ResearchCalendarJobID(config.ID, closed)
	var eventJob, calendarJob occurrence
	for _, job := range schedule.occurrences {
		if job.At.Equal(at) {
			eventJob = job
		}
		if job.At.Equal(at.Add(time.Second)) {
			calendarJob = job
		}
	}
	var submitted []string
	err = runOnceWithHandler(schedule, root, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/research/schedule-occurrences/"+missedID {
			var got contract.ResearchCalendarOccurrence
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil || got != calendar[0] {
				t.Errorf("calendar write differs from coverage plan: %+v / %v", got, err)
			}
			submitted = append(submitted, missedID)
			_ = json.NewEncoder(w).Encode(contract.ResearchCalendarReceipt{
				ResearchCalendarOccurrence: got, JobID: missedID, Status: "missed", RecordedAt: now,
			})
			return
		}
		body, err := io.ReadAll(r.Body)
		var job jobSubmission
		if r.URL.Path != "/v1/research/jobs" || err != nil || json.Unmarshal(body, &job) != nil {
			t.Errorf("unexpected request for path %s", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		expected := map[string][]byte{eventJob.ID: eventJob.Body, calendarJob.ID: calendarJob.Body}[job.ID]
		if string(body) != string(expected) {
			t.Errorf("job %s sent different bytes from its frozen request", job.ID)
		}
		submitted = append(submitted, job.ID)
		_ = json.NewEncoder(w).Encode(map[string]string{"job_id": job.ID})
	})
	if err != nil || !slices.Equal(submitted, []string{missedID, eventJob.ID, calendarJob.ID}) {
		t.Fatalf("mixed schedule produced %v: %v", submitted, err)
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
	err = runOnceWithHandler(schedule, root, func(w http.ResponseWriter, r *http.Request) {
		t.Error("unverified archive produced a request")
		w.WriteHeader(http.StatusBadRequest)
	})
	if err == nil {
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
	if err := Run(ctx, schedule, Config{ControlURL: testControlURL, ArchiveRoot: t.TempDir(), PollInterval: time.Minute}); err != nil {
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
	for _, name := range []string{"valid-research-schedule.json", "valid-research-event-schedule.json"} {
		data, err := os.ReadFile(filepath.Join(root, name))
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
}
