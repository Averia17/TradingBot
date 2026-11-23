package paper_quotes

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"tradingbot/trading-api/internal/contract"
)

func studyBytes(t *testing.T, value any) ([]byte, string) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	return data, hex.EncodeToString(hash[:])
}

func studyFixture(t *testing.T, priced int) (StudyPlan, StudyArchive) {
	t.Helper()
	base := parseExample(t)
	selected := base.Quotes[1]
	plan := StudyPlan{
		Version: "0.1", Source: Source, ID: "fixture-study",
		MinPricedSamples: 2, MinPricedDays: 2, RequestWindowMS: 1000, MaxAgeSeconds: base.Market.MaxAgeSeconds,
		Cells: []StudyCell{
			{ID: "main", RouteID: "fixture:route-a", UnderlyingID: base.Market.UnderlyingID, Side: "BUY",
				IssuerID: "fixture:issuer-a", Session: "regular", ReferenceUSD: "100.00", Wrapper: selected.Wrapper, Settlement: selected.Settlement},
			{ID: "other", RouteID: "fixture:route-b", UnderlyingID: base.Market.UnderlyingID, Side: "BUY",
				IssuerID: "fixture:issuer-a", Session: "regular", ReferenceUSD: "100", Wrapper: selected.Wrapper, Settlement: selected.Settlement},
		},
	}
	archive := StudyArchive{Version: "0.1", Source: Source, StudyID: plan.ID}
	for i := range priced + 2 {
		start := base.Reference.ObservedAt.AddDate(0, 0, i)
		slot := StudySlot{ID: fmt.Sprintf("slot-%03d", i), CellID: "main", Day: start.Format(time.DateOnly), ScheduledAt: start}
		plan.Slots = append(plan.Slots, slot)
		if i == priced+1 {
			continue
		}
		observation := StudyObservation{
			SlotID: slot.ID, RequestedAt: start, ReceivedAt: start.Add(5 * time.Second), Status: "no_quote",
		}
		if i != priced {
			doc := base
			doc.AsOf = observation.ReceivedAt
			doc.Reference.ObservedAt, doc.Reference.ExpiresAt = start, start.Add(30*time.Second)
			q := selected
			q.ID, q.ObservedAt, q.ExpiresAt = slot.ID, start.Add(time.Second), start.Add(12*time.Second)
			q.Fees = []fee{{ID: "embedded", Treatment: "embedded", AmountUSD: "1"}, {
				ID: "external", Treatment: "additional", AmountUSD: fmt.Sprintf("%d.%02d", (i+1)/100, (i+1)%100),
			}}
			doc.Quotes = []quote{q}
			observation.Status = "comparison"
			raw, _ := studyBytes(t, doc)
			observation.Comparison = raw
		}
		archive.Observations = append(archive.Observations, observation)
	}
	start := base.Reference.ObservedAt
	plan.Slots = append(plan.Slots, StudySlot{ID: "uncollected", CellID: "other", Day: start.Format(time.DateOnly), ScheduledAt: start})
	return plan, archive
}

func summarizeFixture(t *testing.T, plan StudyPlan, archive StudyArchive) (StudyReport, error) {
	t.Helper()
	planBytes, planPin := studyBytes(t, plan)
	archive.PlanSHA256 = planPin
	archiveBytes, archivePin := studyBytes(t, archive)
	return SummarizeStudy(planBytes, planPin, archiveBytes, archivePin)
}

func TestStudyCountsMissingAndRefusedWithExactNearestRanks(t *testing.T) {
	plan, archive := studyFixture(t, 20)
	report, err := summarizeFixture(t, plan, archive)
	if err != nil {
		t.Fatal(err)
	}
	main, other := report.Cells[0], report.Cells[1]
	if report.Source != Source || report.ExecutionEnabled || report.MarketEvidence || report.CoverageComplete ||
		main.Expected != 22 || main.Observed != 21 || main.Missing != 1 || main.Priced != 20 ||
		main.Outcomes["no_quote"] != 1 || main.Outcomes["missing"] != 1 || main.PricedDays != 20 {
		t.Fatalf("study coverage = %+v / %+v", report, main)
	}
	if main.CostBPS.P50 == nil || *main.CostBPS.P50 != "10.00000000" ||
		main.CostBPS.P95 == nil || *main.CostBPS.P95 != "19.00000000" || main.CostBPS.P99 != nil ||
		main.LatencyMS.N != 21 || main.LatencyMS.P95 == nil || *main.LatencyMS.P95 != "5000.000000" {
		t.Fatalf("quantiles = %+v / %+v", main.CostBPS, main.LatencyMS)
	}
	if other.Expected != 1 || other.Observed != 0 || other.Missing != 1 || other.CostBPS.P50 != nil ||
		other.LatencyMS.P50 != nil || other.UnsuccessfulObservedRateBPS != nil {
		t.Fatalf("unobserved cell = %+v", other)
	}
}

func TestStudyRequiresStudyPinsAndExactObservationBinding(t *testing.T) {
	plan, archive := studyFixture(t, 2)
	planBytes, pin := studyBytes(t, plan)
	archive.PlanSHA256 = pin
	archiveBytes, archivePin := studyBytes(t, archive)
	if _, err := SummarizeStudy(planBytes, strings.Repeat("f", 64), archiveBytes, archivePin); err == nil {
		t.Fatal("accepted repinned plan with incorrect digest")
	}
	if _, err := SummarizeStudy(planBytes, pin, archiveBytes, strings.Repeat("e", 64)); err == nil {
		t.Fatal("accepted observations with incorrect digest")
	}
	archive.Observations = append(archive.Observations, archive.Observations[0])
	if _, err := summarizeFixture(t, plan, archive); err == nil {
		t.Fatal("duplicate slot inflated sample count")
	}
	archive.Observations = archive.Observations[:len(archive.Observations)-1]
	var comparison document
	if err := json.Unmarshal(archive.Observations[0].Comparison, &comparison); err != nil {
		t.Fatal(err)
	}
	comparison.Reference.USDPerShare = "81"
	archive.Observations[0].Comparison, _ = studyBytes(t, comparison)
	if _, err := summarizeFixture(t, plan, archive); err == nil {
		t.Fatal("accepted different reference notional")
	}
	comparison.Reference.USDPerShare = "80"
	comparison.Quotes[0].ObservedAt = archive.Observations[0].RequestedAt.Add(-time.Second)
	archive.Observations[0].Comparison, _ = studyBytes(t, comparison)
	if _, err := summarizeFixture(t, plan, archive); err == nil {
		t.Fatal("accepted quote observed before its request")
	}
}

func TestStudyRetainsUnpricedComparisonReasons(t *testing.T) {
	for _, example := range []struct {
		reason string
		change func(*document)
	}{
		{"reference_stale", func(d *document) { d.Reference.ExpiresAt = d.AsOf }},
		{"quote_expired", func(d *document) { d.Quotes[0].ExpiresAt = d.AsOf }},
		{"indicative_quote", func(d *document) {
			d.Quotes[0].Mode, d.Quotes[0].MinOutputRaw = "indicative", ""
		}},
	} {
		t.Run(example.reason, func(t *testing.T) {
			plan, archive := studyFixture(t, 2)
			var comparison document
			if err := json.Unmarshal(archive.Observations[0].Comparison, &comparison); err != nil {
				t.Fatal(err)
			}
			example.change(&comparison)
			archive.Observations[0].Comparison, _ = studyBytes(t, comparison)
			report, err := summarizeFixture(t, plan, archive)
			if err != nil {
				t.Fatal(err)
			}
			main := report.Cells[0]
			row := main.Slots[0]
			if row.Outcome != "comparison_excluded" || row.Exclusion != example.reason || row.QuoteID == "" ||
				main.Priced != 1 || main.Exclusions[example.reason] != 1 || main.CostBPS.P50 != nil {
				t.Fatalf("comparison should remain visible without a price: %+v", main)
			}
		})
	}
}

func TestStudyP99HasEnoughResolution(t *testing.T) {
	plan, archive := studyFixture(t, 100)
	report, err := summarizeFixture(t, plan, archive)
	if err != nil {
		t.Fatal(err)
	}
	quantiles := report.Cells[0].CostBPS
	if quantiles.N != 100 || quantiles.P99 == nil || *quantiles.P99 != "99.00000000" {
		t.Fatalf("p99 = %+v", quantiles)
	}
}

func TestPublishedStudyArtifactsMatchRuntimeSchema(t *testing.T) {
	read := func(name string) []byte {
		data, err := os.ReadFile("../../../../contracts/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	if !bytes.Equal(read("paper-quote-study.schema.json"), studySchema) {
		t.Fatal("embedded quote study schema differs from the published schema")
	}
	plan := read("valid-paper-quote-study-plan.json")
	archive := read("valid-paper-quote-study-observations.json")
	planSHA, archiveSHA := sha256.Sum256(plan), sha256.Sum256(archive)
	report, err := SummarizeStudy(plan, hex.EncodeToString(planSHA[:]), archive, hex.EncodeToString(archiveSHA[:]))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := studyBytes(t, report)
	var decoded StudyReport
	if err := contract.Decode(raw, studyReportSchema, &decoded); err != nil {
		t.Fatal(err)
	}
	other, err := SummarizeStudy(plan, hex.EncodeToString(planSHA[:]), archive, hex.EncodeToString(archiveSHA[:]))
	if err != nil {
		t.Fatal(err)
	}
	repeated, _ := studyBytes(t, other)
	if !bytes.Equal(raw, repeated) {
		t.Fatal("repeating the same fixture changed the report")
	}
}
