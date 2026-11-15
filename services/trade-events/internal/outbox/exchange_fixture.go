package outbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"tradingbot/trade-events/internal/fill_event"
)

// PrepareExchangeFixtures enables storage for the simulated exchange endpoint.
func (o *Outbox) PrepareExchangeFixtures(ctx context.Context) error {
	err := pgx.BeginFunc(ctx, o.db.Conn, func(tx pgx.Tx) error {
		const migrationLock = 79077101940501202
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(migrationLock)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS exchange_fixture_fills (
			id text PRIMARY KEY,
			payload bytea NOT NULL,
			recorded_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`)
		return err
	})
	if err != nil {
		return errors.New("migrate PostgreSQL exchange fixtures")
	}
	return nil
}

// RecordExchangeFixture commits a simulated fill and its outbox task together.
// It reports whether this submission created a new fill.
func (o *Outbox) RecordExchangeFixture(ctx context.Context, event fill_event.Event) (bool, error) {
	if event.Source != fill_event.SourceExchangeFixture || event.Mode != "paper" {
		return false, errors.New("exchange fixtures require a simulated paper fill")
	}
	event.FilledAt = event.FilledAt.UTC()
	if err := event.Validate(); err != nil {
		return false, err
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return false, err
	}
	var created bool
	err = pgx.BeginTxFunc(ctx, o.db.Conn, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL synchronous_commit = on"); err != nil {
			return err
		}
		result, err := tx.Exec(ctx, `INSERT INTO exchange_fixture_fills (id, payload)
			VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`, event.ID, payload)
		if err != nil {
			return err
		}
		created = result.RowsAffected() == 1
		if created {
			_, err := o.EnqueueTx(ctx, tx, event)
			return err
		}
		// READ COMMITTED gives this statement a fresh view after a concurrent INSERT.
		var stored []byte
		if err := tx.QueryRow(ctx, "SELECT payload FROM exchange_fixture_fills WHERE id = $1", event.ID).Scan(&stored); err != nil {
			return err
		}
		if !bytes.Equal(stored, payload) {
			return ErrConflict
		}
		return nil
	})
	return err == nil && created, err
}
