package postgres

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrNotFound means the project, sandbox, or user is missing or inaccessible.
// Callers must treat every cause as the same result.
var ErrNotFound = errors.New("not found")

// Position is the creation time and id a later page continues after.
type Position struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// Page is one keyset list read.
// Limit is the number of rows to read.
// A nil After starts at the newest row.
type Page struct {
	Limit int32
	After *Position
}

// Absent maps a missing row to ErrNotFound.
// Any other error is returned unchanged.
func Absent(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// Rollback ends tx when it is still open.
// timeout bounds the rollback used when the request context is already canceled.
func Rollback(tx pgx.Tx, timeout time.Duration) {
	if tx == nil {
		return
	}
	rollbackCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err := tx.Rollback(rollbackCtx)
	if err == nil || errors.Is(err, pgx.ErrTxClosed) {
		return
	}
	log.Printf("internal error: %v", err)
}
