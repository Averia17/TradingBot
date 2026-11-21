package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrDailyBudgetConflict = errors.New("research call limit already configured for this UTC day")

type DailyCallBudget struct {
	Version          string `json:"schema_version"`
	Day              string `json:"utc_day"`
	Status           string `json:"status"`
	MaxCalls         *int64 `json:"max_calls"`
	RecordedPermits  int64  `json:"recorded_permits"`
	RemainingPermits *int64 `json:"remaining_permits"`
}

func lockDailyCallPolicy(ctx context.Context, tx pgx.Tx) (*string, error) {
	var activeFrom *string
	if err := tx.QueryRow(ctx, `SELECT to_char(active_from, 'YYYY-MM-DD')
		FROM research_daily_call_policy WHERE singleton FOR UPDATE`).Scan(&activeFrom); err != nil {
		return nil, fmt.Errorf("lock research daily call policy: %w", err)
	}
	return activeFrom, nil
}

func validBudgetDay(value string) bool {
	day, err := time.Parse(time.DateOnly, value)
	return err == nil && day.Year() > 0 && day.Format(time.DateOnly) == value
}

// SetDailyCallBudget binds an immutable owner limit to a UTC date. The first
// configured day activates the policy, so later unconfigured days block grants.
func (s *Store) SetDailyCallBudget(ctx context.Context, day string, maxCalls int64) error {
	if !validBudgetDay(day) || maxCalls < 0 {
		return errors.New("invalid research call limit or UTC day")
	}
	tx, err := s.beginWrite(ctx)
	if err != nil {
		return fmt.Errorf("begin research daily limit: %w", err)
	}
	defer rollback(tx)
	if _, err := lockDailyCallPolicy(ctx, tx); err != nil {
		return err
	}
	var existing int64
	err = tx.QueryRow(ctx, `SELECT max_calls FROM research_daily_call_limits WHERE utc_day=$1::date`, day).Scan(&existing)
	if err == nil {
		if existing == maxCalls {
			return nil
		}
		return ErrDailyBudgetConflict
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read existing research daily limit: %w", err)
	}
	result, err := tx.Exec(ctx, `INSERT INTO research_daily_call_limits (utc_day, max_calls)
		SELECT $1::date, $2 WHERE $1::date >= (clock_timestamp() AT TIME ZONE 'UTC')::date`, day, maxCalls)
	if err != nil {
		return fmt.Errorf("record research daily limit: %w", err)
	}
	if result.RowsAffected() != 1 {
		return errors.New("cannot configure a new research call limit for a past UTC day")
	}
	if _, err := tx.Exec(ctx, `UPDATE research_daily_call_policy
		SET active_from = LEAST(active_from, $1::date) WHERE singleton`, day); err != nil {
		return fmt.Errorf("activate research daily limits: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit research daily limit: %w", err)
	}
	return nil
}

// ReadDailyCallBudget reports durable permits from one PostgreSQL snapshot and
// can use a SELECT-only reporting role. An empty day uses the database UTC date.
func ReadDailyCallBudget(ctx context.Context, dsn, day string) (DailyCallBudget, error) {
	if day != "" && !validBudgetDay(day) {
		return DailyCallBudget{}, errors.New("invalid research budget UTC day")
	}
	if strings.TrimSpace(dsn) == "" {
		return DailyCallBudget{}, errors.New("missing research PostgreSQL configuration")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		return DailyCallBudget{}, errors.New("invalid research PostgreSQL configuration")
	}
	if config.ConnectTimeout <= 0 || config.ConnectTimeout > 5*time.Second {
		config.ConnectTimeout = 5 * time.Second
	}
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return DailyCallBudget{}, errors.New("connect to research PostgreSQL for budget reporting failed")
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = conn.Close(ctx)
	}()

	report := DailyCallBudget{Version: "0.1"}
	var enforcementActive bool
	err = conn.QueryRow(ctx, `WITH day AS (
		SELECT COALESCE(NULLIF($1, '')::date, (statement_timestamp() AT TIME ZONE 'UTC')::date) AS utc_day
	)
	SELECT to_char(day.utc_day, 'YYYY-MM-DD'), COALESCE(policy.active_from <= day.utc_day, false), limits.max_calls,
		(SELECT count(*) FROM research_call_permits
		 WHERE granted_at >= (day.utc_day::timestamp AT TIME ZONE 'UTC')
		   AND granted_at < (day.utc_day::timestamp AT TIME ZONE 'UTC') + interval '24 hours')
	FROM day CROSS JOIN research_daily_call_policy AS policy
	LEFT JOIN research_daily_call_limits AS limits USING (utc_day)
	WHERE policy.singleton`, day).Scan(&report.Day, &enforcementActive, &report.MaxCalls, &report.RecordedPermits)
	if err != nil {
		return DailyCallBudget{}, fmt.Errorf("read research daily call budget: %w", err)
	}
	switch {
	case !enforcementActive && report.MaxCalls == nil:
		report.Status = "unconfigured"
	case !enforcementActive:
		return DailyCallBudget{}, errors.New("inconsistent research daily call policy")
	case report.MaxCalls == nil:
		report.Status = "missing_limit"
		report.RemainingPermits = new(int64)
	default:
		report.Status = "available"
		remaining := max(0, *report.MaxCalls-report.RecordedPermits)
		report.RemainingPermits = &remaining
		if remaining == 0 {
			report.Status = "exhausted"
		}
	}
	return report, nil
}
