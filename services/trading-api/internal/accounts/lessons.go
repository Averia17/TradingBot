package accounts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

const LessonMethod = "fifo_sale_exit_fee_v1"

var (
	lessonDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
	lessonAmount = regexp.MustCompile(`^-?(?:0|[1-9][0-9]{0,59})(?:\.[0-9]{1,8})?$`)
)

type LessonFact struct {
	AccountID            string     `json:"account_id"`
	SourceFillEventID    string     `json:"source_fill_event_id"`
	SourceDecisionID     string     `json:"source_decision_id"`
	SourceAccountVersion int64      `json:"source_account_version"`
	SourceOutcomeSHA256  string     `json:"source_outcome_sha256"`
	SourceAvailableAt    time.Time  `json:"source_available_at"`
	LabelEnd             time.Time  `json:"label_end"`
	Instrument           Instrument `json:"instrument"`
	QuantityRaw          string     `json:"quantity_raw"`
	GrossUSD             string     `json:"gross_usd"`
	RecordedExitFeeUSD   string     `json:"recorded_exit_fee_usd"`
	CostBasisUSD         string     `json:"cost_basis_usd"`
	NetPnLUSD            string     `json:"net_pnl_usd"`
	RealizedOutcome      string     `json:"realized_outcome"`
	ExitFeeErasedGain    bool       `json:"exit_fee_erased_positive_pre_fee_pnl"`
}

type LessonEvent struct {
	Version     string     `json:"schema_version"`
	SourceMode  string     `json:"source_mode"`
	Method      string     `json:"method"`
	LessonID    string     `json:"lesson_id"`
	EventID     string     `json:"event_id"`
	Revision    int        `json:"revision"`
	Action      string     `json:"action"`
	CreatedAt   time.Time  `json:"created_at"`
	AvailableAt time.Time  `json:"available_at"`
	Fact        LessonFact `json:"fact"`
}

func (f LessonFact) lessonID() string {
	digest := sha256.Sum256([]byte(LessonMethod + ":" + f.AccountID + ":" + f.SourceFillEventID))
	return hex.EncodeToString(digest[:])
}

// Classify verifies the exact realized cash flow and derives the two lesson
// labels from those amounts. It does not change the economic fields.
func (f *LessonFact) Classify() error {
	if !moneyPattern.MatchString(f.GrossUSD) || !moneyPattern.MatchString(f.RecordedExitFeeUSD) ||
		!lessonAmount.MatchString(f.CostBasisUSD) || !lessonAmount.MatchString(f.NetPnLUSD) {
		return ErrInvalid
	}
	gross, _ := new(big.Rat).SetString(f.GrossUSD)
	fee, _ := new(big.Rat).SetString(f.RecordedExitFeeUSD)
	basis, _ := new(big.Rat).SetString(f.CostBasisUSD)
	net, _ := new(big.Rat).SetString(f.NetPnLUSD)
	beforeFee := new(big.Rat).Sub(gross, basis)
	if gross.Sign() <= 0 || gross.Cmp(fee) < 0 || basis.Sign() < 0 || new(big.Rat).Sub(beforeFee, fee).Cmp(net) != 0 {
		return ErrInvalid
	}
	f.RealizedOutcome = "break_even"
	switch net.Sign() {
	case -1:
		f.RealizedOutcome = "loss"
	case 1:
		f.RealizedOutcome = "gain"
	}
	f.ExitFeeErasedGain = beforeFee.Sign() > 0 && net.Sign() <= 0
	return nil
}

// RecordLesson stores the FIFO-derived fact once and binds its first receipt in
// the same transaction. The fact must refer to the actual SELL fill in this
// account. The derived cost basis is calculated by the paper projection.
func (s *Store) RecordLesson(ctx context.Context, fact LessonFact, eventID string) (LessonEvent, error) {
	if !uuidPattern.MatchString(fact.AccountID) || !uuidPattern.MatchString(fact.SourceFillEventID) ||
		!uuidPattern.MatchString(fact.SourceDecisionID) || !uuidPattern.MatchString(eventID) ||
		!lessonDigest.MatchString(fact.SourceOutcomeSHA256) || !rawPattern.MatchString(fact.QuantityRaw) || fact.SourceAccountVersion < 2 ||
		fact.LabelEnd.IsZero() || fact.LabelEnd.After(fact.SourceAvailableAt) {
		return LessonEvent{}, ErrInvalid
	}
	classified := fact
	if err := classified.Classify(); err != nil || classified.RealizedOutcome != fact.RealizedOutcome ||
		classified.ExitFeeErasedGain != fact.ExitFeeErasedGain {
		return LessonEvent{}, ErrInvalid
	}
	fact.LabelEnd, fact.SourceAvailableAt = fact.LabelEnd.UTC(), fact.SourceAvailableAt.UTC()
	raw, err := json.Marshal(fact)
	if err != nil || len(raw) > 8192 {
		return LessonEvent{}, ErrInvalid
	}
	id := fact.lessonID()
	tx, err := s.write(ctx)
	if err != nil {
		return LessonEvent{}, err
	}
	defer rollback(tx)
	if _, err := lockAccount(ctx, tx, fact.AccountID); err != nil {
		return LessonEvent{}, err
	}
	prior, priorFact, err := readLessonEvent(ctx, tx, fact.AccountID, eventID)
	if err == nil {
		if prior.Revision != 1 || prior.LessonID != id || !bytes.Equal(priorFact, raw) {
			return LessonEvent{}, ErrConflict
		}
		return prior, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return LessonEvent{}, err
	}
	if err := checkLessonSource(ctx, tx, fact); err != nil {
		return LessonEvent{}, err
	}
	inserted, err := tx.Exec(ctx, `INSERT INTO paper_memory_lessons (account_id, lesson_id, source_fill_event_id, fact)
		VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING`, fact.AccountID, id, fact.SourceFillEventID, raw)
	if err != nil {
		return LessonEvent{}, fmt.Errorf("record paper memory fact: %w", err)
	}
	if inserted.RowsAffected() != 1 {
		return LessonEvent{}, ErrConflict
	}
	return commitLessonEvent(ctx, tx, fact, id, eventID, "record", 1, fact.SourceAvailableAt)
}

func checkLessonSource(ctx context.Context, tx pgx.Tx, fact LessonFact) error {
	var matches bool
	err := tx.QueryRow(ctx, `SELECT e.account_version=$3 AND o.decision_id=$4 AND f.observed_at=$5
		AND e.recorded_at<=$6 AND $6<=clock_timestamp() AND f.gross_usd=$7::text::numeric
		AND f.fee_usd=$8::text::numeric AND f.quantity_raw::text=$9 AND o.request->>'side'='SELL'
		AND o.request->'instrument'->>'instrument_id'=$10 AND o.request->'instrument'->>'registry_version'=$11
		AND o.request->'instrument'->>'decimals'=$12
		FROM paper_fills f JOIN paper_events e USING(account_id,event_id)
		JOIN paper_orders o USING(account_id,order_id) WHERE f.account_id=$1 AND f.event_id=$2`,
		fact.AccountID, fact.SourceFillEventID, fact.SourceAccountVersion, fact.SourceDecisionID,
		fact.LabelEnd, fact.SourceAvailableAt, fact.GrossUSD, fact.RecordedExitFeeUSD, fact.QuantityRaw,
		fact.Instrument.ID, fact.Instrument.RegistryVersion, fmt.Sprint(fact.Instrument.Decimals)).Scan(&matches)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !matches {
		return ErrInvalid
	}
	if err != nil {
		return fmt.Errorf("verify source for paper memory fact: %w", err)
	}
	return nil
}

// CorrectLesson appends an activation change without replacing the original
// amounts or prior history. Expected revision prevents concurrent corrections
// from silently superseding one another.
func (s *Store) CorrectLesson(ctx context.Context, accountID, id, eventID, action string, expected int) (LessonEvent, error) {
	if !uuidPattern.MatchString(accountID) || !uuidPattern.MatchString(eventID) || !lessonDigest.MatchString(id) ||
		(action != "retract" && action != "reinstate") || expected < 1 || expected >= 1<<31-1 {
		return LessonEvent{}, ErrInvalid
	}
	tx, err := s.write(ctx)
	if err != nil {
		return LessonEvent{}, err
	}
	defer rollback(tx)
	if _, err := lockAccount(ctx, tx, accountID); err != nil {
		return LessonEvent{}, err
	}
	prior, _, err := readLessonEvent(ctx, tx, accountID, eventID)
	if err == nil {
		if prior.Revision != expected+1 || prior.LessonID != id || prior.Action != action {
			return LessonEvent{}, ErrConflict
		}
		return prior, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return LessonEvent{}, err
	}
	prior, _, err = scanLessonEvent(tx.QueryRow(ctx, `SELECT `+lessonColumns+` FROM paper_memory_lesson_events e
		JOIN paper_memory_lessons l USING(account_id,lesson_id) WHERE e.account_id=$1 AND lesson_id=$2
		ORDER BY e.revision DESC LIMIT 1`, accountID, id))
	if err != nil {
		return LessonEvent{}, err
	}
	if prior.Revision != expected || (action == "reinstate") != (prior.Action == "retract") {
		return LessonEvent{}, ErrConflict
	}
	return commitLessonEvent(ctx, tx, prior.Fact, id, eventID, action, expected+1, prior.AvailableAt.Add(time.Microsecond))
}

func commitLessonEvent(ctx context.Context, tx pgx.Tx, fact LessonFact, id, eventID, action string, revision int, earliest time.Time) (LessonEvent, error) {
	var created, available time.Time
	err := tx.QueryRow(ctx, `WITH timing AS (SELECT clock_timestamp() AS now)
		INSERT INTO paper_memory_lesson_events (account_id,lesson_id,event_id,revision,action,created_at,available_at)
		SELECT $1,$2,$3,$4,$5,now,greatest(now,$6::timestamptz) FROM timing
		RETURNING created_at,available_at`, fact.AccountID, id, eventID, revision, action, earliest).Scan(&created, &available)
	if err != nil {
		return LessonEvent{}, fmt.Errorf("append paper memory event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return LessonEvent{}, fmt.Errorf("commit paper memory event: %w", err)
	}
	return LessonEvent{Version: "0.1", SourceMode: "paper_simulated_ledger", Method: LessonMethod,
		LessonID: id, EventID: eventID, Revision: revision, Action: action,
		CreatedAt: created.UTC(), AvailableAt: available.UTC(), Fact: fact}, nil
}

const lessonColumns = `e.lesson_id,e.event_id::text,e.revision,e.action,e.created_at,e.available_at,l.fact`

func readLessonEvent(ctx context.Context, tx pgx.Tx, accountID, eventID string) (LessonEvent, []byte, error) {
	return scanLessonEvent(tx.QueryRow(ctx, `SELECT `+lessonColumns+` FROM paper_memory_lesson_events e
		JOIN paper_memory_lessons l USING(account_id,lesson_id) WHERE e.account_id=$1 AND e.event_id=$2`, accountID, eventID))
}

func scanLessonEvent(row pgx.Row) (LessonEvent, []byte, error) {
	event := LessonEvent{Version: "0.1", SourceMode: "paper_simulated_ledger", Method: LessonMethod}
	var raw []byte
	err := row.Scan(&event.LessonID, &event.EventID, &event.Revision, &event.Action, &event.CreatedAt, &event.AvailableAt, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return LessonEvent{}, nil, ErrNotFound
	}
	if err != nil {
		return LessonEvent{}, nil, fmt.Errorf("read paper memory event: %w", err)
	}
	if err := json.Unmarshal(raw, &event.Fact); err != nil || event.Fact.lessonID() != event.LessonID {
		return LessonEvent{}, nil, errors.New("invalid stored paper memory fact")
	}
	event.CreatedAt, event.AvailableAt = event.CreatedAt.UTC(), event.AvailableAt.UTC()
	return event, raw, nil
}

// LessonForFill returns the latest visible event, including a retraction,
// for the original fill in this consistent ledger snapshot.
func (s ReadSnapshot) LessonForFill(ctx context.Context, accountID, fillEventID string, asOf time.Time) (LessonEvent, error) {
	if !uuidPattern.MatchString(accountID) || !uuidPattern.MatchString(fillEventID) || asOf.IsZero() || asOf.After(s.cutoff) {
		return LessonEvent{}, ErrInvalid
	}
	event, _, err := scanLessonEvent(s.tx.QueryRow(ctx, `SELECT `+lessonColumns+` FROM paper_memory_lessons l
		JOIN paper_memory_lesson_events e USING(account_id,lesson_id)
		WHERE l.account_id=$1 AND l.source_fill_event_id=$2 AND e.available_at<=$3
		ORDER BY e.revision DESC LIMIT 1`, accountID, fillEventID, asOf))
	return event, err
}

// Lessons reads the version that was visible at the cutoff, including a
// retracted status when that is its last visible event. History returns earlier
// events as well. A zero cutoff selects the current database timestamp.
func (s *Store) Lessons(ctx context.Context, accountID, id string, asOf time.Time, history bool, limit int) ([]LessonEvent, error) {
	if !uuidPattern.MatchString(accountID) || id != "" && !lessonDigest.MatchString(id) || limit < 1 || limit > 100 {
		return nil, ErrInvalid
	}
	var now time.Time
	if err := s.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return nil, fmt.Errorf("read paper memory clock: %w", err)
	}
	if asOf.IsZero() {
		asOf = now
	}
	if asOf.After(now) {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `WITH visible AS (
		SELECT *,row_number() OVER(PARTITION BY lesson_id ORDER BY revision DESC) AS rank
		FROM paper_memory_lesson_events WHERE account_id=$1 AND available_at<=$2 AND ($3='' OR lesson_id=$3)
	) SELECT `+lessonColumns+` FROM visible e JOIN paper_memory_lessons l USING(account_id,lesson_id)
	WHERE $4 OR e.rank=1 ORDER BY e.available_at DESC,e.lesson_id,e.revision DESC LIMIT $5`, accountID, asOf, id, history, limit)
	if err != nil {
		return nil, fmt.Errorf("list paper memory events: %w", err)
	}
	defer rows.Close()
	events := []LessonEvent{}
	for rows.Next() {
		event, _, err := scanLessonEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan paper memory events: %w", err)
	}
	return events, nil
}
