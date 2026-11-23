package accounts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const maxApplicationDocument = 256 * 1024

type ApplicationInput struct {
	JobID           string
	AccountID       string
	SnapshotVersion int64
	RequestSHA256   [sha256.Size]byte
	Quote           []byte
	ReserveBefore   time.Time
}

type ApplicationRecord struct {
	ApplicationInput
	OrderID string
	Result  []byte
}

func validApplication(input ApplicationInput) bool {
	return uuidPattern.MatchString(input.JobID) && uuidPattern.MatchString(input.AccountID) &&
		input.SnapshotVersion > 0 && len(input.Quote) <= maxApplicationDocument
}

func (a ApplicationRecord) matches(input ApplicationInput, orderID string) bool {
	return a.JobID == input.JobID && a.AccountID == input.AccountID && a.SnapshotVersion == input.SnapshotVersion &&
		a.RequestSHA256 == input.RequestSHA256 && a.OrderID == orderID && bytes.Equal(a.Quote, input.Quote)
}

func (s *Store) GetApplication(ctx context.Context, jobID string) (ApplicationRecord, error) {
	if !uuidPattern.MatchString(jobID) {
		return ApplicationRecord{}, ErrInvalid
	}
	return readApplication(ctx, s.pool, jobID)
}

// ApplicationAfterReservations checks for an admitted application after any
// in-flight account reservation finishes.
func (s *Store) ApplicationAfterReservations(ctx context.Context, accountID, jobID string) (ApplicationRecord, error) {
	if !uuidPattern.MatchString(accountID) || !uuidPattern.MatchString(jobID) {
		return ApplicationRecord{}, ErrInvalid
	}
	tx, err := s.write(ctx)
	if err != nil {
		return ApplicationRecord{}, err
	}
	defer rollback(tx)
	if _, err := lockAccount(ctx, tx, accountID); err != nil {
		return ApplicationRecord{}, err
	}
	return readApplication(ctx, tx, jobID)
}

func readApplication(ctx context.Context, db rowQuerier, jobID string) (ApplicationRecord, error) {
	var record ApplicationRecord
	var requestHash, quoteHash, resultHash []byte
	err := db.QueryRow(ctx, `SELECT account_id::text, snapshot_version, request_sha256, coalesce(order_id::text, ''),
		quote, quote_sha256, result, result_sha256 FROM paper_applications WHERE job_id=$1`, jobID).
		Scan(&record.AccountID, &record.SnapshotVersion, &requestHash, &record.OrderID, &record.Quote, &quoteHash, &record.Result, &resultHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return ApplicationRecord{}, ErrNotFound
	}
	if err != nil {
		return ApplicationRecord{}, fmt.Errorf("read paper application: %w", err)
	}
	if len(requestHash) != sha256.Size || !bytes.Equal(quoteHash, documentHash(record.Quote)) ||
		!bytes.Equal(resultHash, documentHash(record.Result)) {
		return ApplicationRecord{}, errors.New("stored paper application does not match its hashes")
	}
	record.JobID = jobID
	copy(record.RequestSHA256[:], requestHash)
	return record, nil
}

func documentHash(body []byte) []byte {
	if len(body) == 0 {
		return nil
	}
	digest := sha256.Sum256(body)
	return digest[:]
}

func bindApplication(ctx context.Context, tx pgx.Tx, input ApplicationInput, orderID string, result []byte) (ApplicationRecord, error) {
	_, err := tx.Exec(ctx, `INSERT INTO paper_applications
		(job_id, account_id, snapshot_version, request_sha256, order_id, quote, quote_sha256, result, result_sha256)
		VALUES ($1,$2,$3,$4,nullif($5,'')::uuid,$6,$7,$8,$9) ON CONFLICT (job_id) DO NOTHING`, input.JobID, input.AccountID,
		input.SnapshotVersion, input.RequestSHA256[:], orderID, input.Quote, documentHash(input.Quote), result, documentHash(result))
	if err != nil {
		return ApplicationRecord{}, fmt.Errorf("bind paper application: %w", err)
	}
	record, err := readApplication(ctx, tx, input.JobID)
	if err != nil {
		return ApplicationRecord{}, err
	}
	if !record.matches(input, orderID) {
		return ApplicationRecord{}, ErrConflict
	}
	return record, nil
}

// RecordApplication preserves the first complete receipt. A HOLD is recorded
// only while its account version and decision window are current.
func (s *Store) RecordApplication(ctx context.Context, input ApplicationInput, orderID string, result []byte, expiresAt time.Time) (ApplicationRecord, error) {
	if !validApplication(input) || len(result) > maxApplicationDocument || !json.Valid(result) ||
		(orderID == "") != (len(input.Quote) == 0) || (orderID != "" && !uuidPattern.MatchString(orderID)) {
		return ApplicationRecord{}, ErrInvalid
	}
	tx, err := s.write(ctx)
	if err != nil {
		return ApplicationRecord{}, err
	}
	defer rollback(tx)
	version, err := lockAccount(ctx, tx, input.AccountID)
	if err != nil {
		return ApplicationRecord{}, err
	}
	record, err := readApplication(ctx, tx, input.JobID)
	if errors.Is(err, ErrNotFound) && orderID == "" {
		if version != input.SnapshotVersion {
			return ApplicationRecord{}, ErrStale
		}
		var fresh bool
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp() < $1::timestamptz`, expiresAt.UTC()).Scan(&fresh); err != nil {
			return ApplicationRecord{}, fmt.Errorf("check paper HOLD cutoff: %w", err)
		}
		if !fresh {
			return ApplicationRecord{}, ErrExpired
		}
		record, err = bindApplication(ctx, tx, input, "", result)
	}
	if err != nil {
		return ApplicationRecord{}, err
	}
	if !record.matches(input, orderID) {
		return ApplicationRecord{}, ErrConflict
	}
	if len(record.Result) == 0 {
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM paper_orders WHERE account_id=$1 AND order_id=$2`,
			input.AccountID, orderID).Scan(&status); err != nil {
			return ApplicationRecord{}, fmt.Errorf("read paper application order: %w", err)
		}
		if status == "reserved" {
			return ApplicationRecord{}, ErrConflict
		}
		if _, err := tx.Exec(ctx, `UPDATE paper_applications SET result=$1, result_sha256=$2 WHERE job_id=$3`,
			result, documentHash(result), input.JobID); err != nil {
			return ApplicationRecord{}, fmt.Errorf("record paper application receipt: %w", err)
		}
		record.Result = result
	}
	if err := tx.Commit(ctx); err != nil {
		return ApplicationRecord{}, fmt.Errorf("commit paper application receipt: %w", err)
	}
	return record, nil
}
