// Package postgres classifies database failures that mean the request
// cannot be completed.
package postgres

import (
	"context"
	"errors"
	"net"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
)

// Unavailable reports that a database call did not finish because the
// connection, the server, or the request context failed.
func Unavailable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr == nil {
		return false
	}
	return pgerrcode.IsConnectionException(pgErr.Code) ||
		pgerrcode.IsInsufficientResources(pgErr.Code) ||
		pgErr.Code == pgerrcode.AdminShutdown ||
		pgErr.Code == pgerrcode.CrashShutdown ||
		pgErr.Code == pgerrcode.CannotConnectNow
}
