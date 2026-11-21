package outbox

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestEnqueueComparesContentsOnlyOnDuplicate(t *testing.T) {
	storageFailure := errors.New("storage failure")
	for _, tc := range []struct {
		name       string
		insertErr  error
		same       bool
		wantID     int64
		wantErr    error
		statements int
	}{
		{name: "new task", wantID: 7, statements: 1},
		{name: "retry", insertErr: pgx.ErrNoRows, same: true, wantID: 7, statements: 2},
		{name: "changed payload", insertErr: pgx.ErrNoRows, wantErr: ErrConflict, statements: 2},
		{name: "insert failure", insertErr: storageFailure, wantErr: storageFailure, statements: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := []byte(`{"id":"fill"}`)
			calls := 0
			tx := enqueueTxStub{queryRow: func(sql string, args ...any) pgx.Row {
				calls++
				if len(args) < 3 || args[0] != queueName || args[1] != "fill" || !bytes.Equal(args[2].([]byte), payload) {
					t.Fatalf("unexpected task identity or payload: %v", args)
				}
				switch calls {
				case 1:
					if !strings.Contains(sql, "DO NOTHING RETURNING id") {
						t.Fatal("new task must return its ID from the insert")
					}
					return rowScanner(func(dest ...any) error {
						if tc.insertErr == nil {
							*dest[0].(*int64) = 7
						}
						return tc.insertErr
					})
				case 2:
					if !strings.Contains(sql, "payload IS NOT DISTINCT FROM") {
						t.Fatal("duplicate must compare the original bytes")
					}
					return rowScanner(func(dest ...any) error {
						*dest[0].(*int64) = 7
						*dest[1].(*bool) = tc.same
						return nil
					})
				default:
					t.Fatal("unexpected query after duplicate lookup")
					return nil
				}
			}}
			id, err := enqueueExactTask(t.Context(), tx, queueName, "fill", payload)
			if id != tc.wantID || !errors.Is(err, tc.wantErr) || calls != tc.statements {
				t.Fatalf("id=%d, statements=%d, err=%v", id, calls, err)
			}
		})
	}
}

type enqueueTxStub struct {
	pgx.Tx
	queryRow func(string, ...any) pgx.Row
}

func (tx enqueueTxStub) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	return tx.queryRow(sql, args...)
}

type rowScanner func(...any) error

func (f rowScanner) Scan(dest ...any) error { return f(dest...) }
