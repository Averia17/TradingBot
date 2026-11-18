package ledger

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type ReadSnapshot struct {
	tx     pgx.Tx
	cutoff time.Time
}

// Cutoff is the database transaction timestamp for this consistent read.
func (s ReadSnapshot) Cutoff() time.Time { return s.cutoff }

func (s ReadSnapshot) Events(ctx context.Context, accountID string, afterVersion int64, limit int) ([]Event, error) {
	return readEvents(ctx, s.tx, accountID, afterVersion, limit)
}

func (s ReadSnapshot) GetOrder(ctx context.Context, accountID, orderID string) (Order, error) {
	return getOrder(ctx, s.tx, accountID, orderID)
}

func (s ReadSnapshot) GetApplication(ctx context.Context, jobID string) (ApplicationRecord, error) {
	if !uuidPattern.MatchString(jobID) {
		return ApplicationRecord{}, ErrInvalid
	}
	return readApplication(ctx, s.tx, jobID)
}

// WithReadSnapshot runs a ledger query in a read-only, repeatable-read
// transaction. It allows a reporting role with SELECT access and no migrations.
func WithReadSnapshot(ctx context.Context, dsn string, read func(ReadSnapshot) error) error {
	if dsn == "" {
		return errors.New("missing paper PostgreSQL configuration")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		return errors.New("invalid paper PostgreSQL configuration")
	}
	if config.ConnectTimeout <= 0 || config.ConnectTimeout > 5*time.Second {
		config.ConnectTimeout = 5 * time.Second
	}
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return errors.New("connect to paper PostgreSQL for reporting failed")
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = conn.Close(ctx)
	}()
	var readErr error
	err = pgx.BeginTxFunc(ctx, conn, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var cutoff time.Time
		if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&cutoff); err != nil {
			return err
		}
		readErr = read(ReadSnapshot{tx: tx, cutoff: cutoff.UTC()})
		return readErr
	})
	if readErr != nil {
		return readErr
	}
	if err != nil {
		return errors.New("read-only paper ledger transaction failed")
	}
	return nil
}
