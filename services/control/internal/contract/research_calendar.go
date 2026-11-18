package contract

import (
	"crypto/sha256"
	_ "embed"
	"fmt"
	"time"
)

//go:embed research-calendar-occurrence.schema.json
var researchCalendarSchema []byte

var ResearchCalendarOccurrenceInput = compile(researchCalendarSchema, "#/$defs/Occurrence")
var ResearchCalendarOccurrenceReceipt = compile(researchCalendarSchema, "#/$defs/Receipt")

const ResearchCalendarObservationLookback = 24 * time.Hour

type ResearchCalendarOccurrence struct {
	Version            string    `json:"schema_version"`
	ScheduleID         string    `json:"schedule_id"`
	ScheduledAt        time.Time `json:"scheduled_at"`
	MaxLatenessSeconds int       `json:"max_lateness_seconds"`
	RequestSHA256      string    `json:"request_sha256"`
}

func (o ResearchCalendarOccurrence) Deadline() time.Time {
	return o.ScheduledAt.Add(time.Duration(o.MaxLatenessSeconds) * time.Second)
}

type ResearchCalendarReceipt struct {
	ResearchCalendarOccurrence
	JobID        string     `json:"job_id"`
	RecordedAt   time.Time  `json:"recorded_at"`
	Status       string     `json:"status"`
	JobCreatedAt *time.Time `json:"job_created_at,omitempty"`
}

// ResearchCalendarJobID is stable across changes to a scheduled job's inputs.
func ResearchCalendarJobID(scheduleID string, at time.Time) string {
	return DerivedResearchJobID("tradingbot/research-calendar-v1\n" + scheduleID + "\n" + at.UTC().Format(time.RFC3339Nano))
}

// DerivedResearchJobID uses the existing SHA-256-based UUIDv8 job identity.
func DerivedResearchJobID(key string) string {
	hash := sha256.Sum256([]byte(key))
	hash[6] = hash[6]&0x0f | 0x80
	hash[8] = hash[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", hash[:4], hash[4:6], hash[6:8], hash[8:10], hash[10:16])
}
