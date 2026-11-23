package accounts

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	outboxPath     = "/internal/v1/paper/outbox"
	outboxPageSize = 128
)

type outboxEntry struct {
	AccountID      string          `json:"account_id"`
	EventID        string          `json:"event_id"`
	AccountVersion int64           `json:"account_version,omitempty"`
	Payload        json.RawMessage `json:"payload"`
}

type outboxHead struct {
	Version        string    `json:"version"`
	AccountID      string    `json:"account_id"`
	AccountVersion int64     `json:"account_version"`
	ObservedAt     time.Time `json:"observed_at"`
}

// NewOutboxHandler exposes committed events for a relay that has no access to PostgreSQL.
// Each acknowledgement is sent after the relay commits the corresponding deduplicated task.
func NewOutboxHandler(store *Store, token string) http.Handler {
	if token == "" {
		return http.NotFoundHandler()
	}
	h := outboxHTTP{store: store}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+outboxPath+"/fills/next", h.nextFill)
	mux.HandleFunc("POST "+outboxPath+"/fills/{account}/{event}/ack", h.ackFill)
	mux.HandleFunc("GET "+outboxPath+"/accounts/next", h.nextAccount)
	mux.HandleFunc("POST "+outboxPath+"/accounts/{account}/{version}/{event}/ack", h.ackAccount)
	mux.HandleFunc("GET "+outboxPath+"/accounts/{account}", h.accountHistory)
	mux.HandleFunc("GET "+outboxPath+"/heads", h.heads)
	mux.HandleFunc("GET "+outboxPath+"/heads/{account}", h.head)
	authorization := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), authorization) != 1 {
			http.Error(w, "paper outbox authentication required", http.StatusUnauthorized)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
}

type outboxHTTP struct {
	store *Store
}

func (h outboxHTTP) nextFill(w http.ResponseWriter, r *http.Request) {
	var entry outboxEntry
	err := h.store.pool.QueryRow(r.Context(), `SELECT account_id::text,event_id::text,payload
		FROM paper_fill_outbox WHERE queued_at IS NULL ORDER BY created_at,account_id,event_id LIMIT 1`).
		Scan(&entry.AccountID, &entry.EventID, &entry.Payload)
	writeNextOutbox(w, entry, err)
}

func (h outboxHTTP) nextAccount(w http.ResponseWriter, r *http.Request) {
	entry, err := scanOutboxAccount(h.store.pool.QueryRow(r.Context(), `SELECT account_id::text,event_id::text,account_version,payload
		FROM paper_account_outbox WHERE queued_at IS NULL ORDER BY created_at,account_id,account_version LIMIT 1`))
	writeNextOutbox(w, entry, err)
}

func writeNextOutbox(w http.ResponseWriter, entry outboxEntry, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeOutboxJSON(w, entry, err)
}

func (h outboxHTTP) ackFill(w http.ResponseWriter, r *http.Request) {
	account, event := r.PathValue("account"), r.PathValue("event")
	if !uuidPattern.MatchString(account) || !uuidPattern.MatchString(event) {
		http.Error(w, "invalid paper fill identity", http.StatusBadRequest)
		return
	}
	h.ack(w, r, `UPDATE paper_fill_outbox SET queued_at=COALESCE(queued_at,clock_timestamp())
		WHERE account_id=$1 AND event_id=$2`, account, event)
}

func (h outboxHTTP) ackAccount(w http.ResponseWriter, r *http.Request) {
	account, event := r.PathValue("account"), r.PathValue("event")
	version, err := strconv.ParseInt(r.PathValue("version"), 10, 64)
	if err != nil || version < 1 || !uuidPattern.MatchString(account) || !uuidPattern.MatchString(event) {
		http.Error(w, "invalid paper account event identity", http.StatusBadRequest)
		return
	}
	h.ack(w, r, `UPDATE paper_account_outbox SET queued_at=COALESCE(queued_at,clock_timestamp())
		WHERE account_id=$1 AND event_id=$2 AND account_version=$3`, account, event, version)
}

func (h outboxHTTP) ack(w http.ResponseWriter, r *http.Request, query string, args ...any) {
	ctx := r.Context()
	tx, err := h.store.write(ctx)
	if err != nil {
		writeOutboxError(w, err)
		return
	}
	defer rollback(tx)
	tag, err := tx.Exec(ctx, query, args...)
	if err != nil {
		writeOutboxError(w, err)
		return
	}
	if tag.RowsAffected() != 1 {
		writeOutboxError(w, pgx.ErrNoRows)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeOutboxError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h outboxHTTP) accountHistory(w http.ResponseWriter, r *http.Request) {
	account := r.PathValue("account")
	query := r.URL.Query()
	after, afterErr := strconv.ParseInt(query.Get("after"), 10, 64)
	through, throughErr := strconv.ParseInt(query.Get("through"), 10, 64)
	if !uuidPattern.MatchString(account) || afterErr != nil || throughErr != nil || after < 0 || through <= after {
		http.Error(w, "invalid paper account history range", http.StatusBadRequest)
		return
	}
	rows, err := h.store.pool.Query(r.Context(), `SELECT account_id::text,event_id::text,account_version,payload
		FROM paper_account_outbox WHERE account_id=$1 AND account_version>$2 AND account_version<=$3
		ORDER BY account_version LIMIT $4`, account, after, through, outboxPageSize)
	if err != nil {
		writeOutboxError(w, err)
		return
	}
	entries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (outboxEntry, error) {
		return scanOutboxAccount(row)
	})
	writeOutboxJSON(w, entries, err)
}

func scanOutboxAccount(row pgx.Row) (outboxEntry, error) {
	var entry outboxEntry
	err := row.Scan(&entry.AccountID, &entry.EventID, &entry.AccountVersion, &entry.Payload)
	return entry, err
}

func (h outboxHTTP) heads(w http.ResponseWriter, r *http.Request) {
	after := r.URL.Query().Get("after")
	if after != "" && !uuidPattern.MatchString(after) {
		http.Error(w, "invalid paper account head cursor", http.StatusBadRequest)
		return
	}
	rows, err := h.store.pool.Query(r.Context(), `SELECT account_id::text,account_version,statement_timestamp()
		FROM paper_account_analytics_heads WHERE ($1='' OR account_id > NULLIF($1,'')::uuid)
		ORDER BY account_id LIMIT $2`, after, outboxPageSize)
	if err != nil {
		writeOutboxError(w, err)
		return
	}
	entries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (outboxHead, error) {
		return scanOutboxHead(row)
	})
	writeOutboxJSON(w, entries, err)
}

func (h outboxHTTP) head(w http.ResponseWriter, r *http.Request) {
	account := r.PathValue("account")
	if !uuidPattern.MatchString(account) {
		http.Error(w, "invalid paper account identity", http.StatusBadRequest)
		return
	}
	entry, err := scanOutboxHead(h.store.pool.QueryRow(r.Context(), `SELECT account_id::text,account_version,statement_timestamp()
		FROM paper_account_analytics_heads WHERE account_id=$1`, account))
	writeOutboxJSON(w, entry, err)
}

func scanOutboxHead(row pgx.Row) (outboxHead, error) {
	head := outboxHead{Version: "1.0"}
	err := row.Scan(&head.AccountID, &head.AccountVersion, &head.ObservedAt)
	head.ObservedAt = head.ObservedAt.UTC()
	return head, err
}

func writeOutboxError(w http.ResponseWriter, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "paper outbox record not found", http.StatusNotFound)
		return
	}
	http.Error(w, "paper outbox unavailable", http.StatusServiceUnavailable)
}

func writeOutboxJSON(w http.ResponseWriter, value any, err error) {
	if err != nil {
		writeOutboxError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
