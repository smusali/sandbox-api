package postgres

import (
	"context"
	"fmt"
	"net"
	"testing"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestUnavailable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "missing row", err: pgx.ErrNoRows, want: false},
		{name: "deadline", err: context.DeadlineExceeded, want: true},
		{name: "canceled", err: context.Canceled, want: true},
		{name: "wrapped deadline", err: fmt.Errorf("query: %w", context.DeadlineExceeded), want: true},
		{name: "network", err: &net.DNSError{IsTimeout: true}, want: true},
		{name: "wrapped network", err: fmt.Errorf("dial: %w", &net.DNSError{IsTimeout: true}), want: true},
		{name: "connection failure", err: &pgconn.PgError{Code: pgerrcode.ConnectionFailure}, want: true},
		{name: "wrapped connection failure", err: fmt.Errorf("query: %w", &pgconn.PgError{Code: pgerrcode.ConnectionFailure}), want: true},
		{name: "disk full", err: &pgconn.PgError{Code: pgerrcode.DiskFull}, want: true},
		{name: "admin shutdown", err: &pgconn.PgError{Code: pgerrcode.AdminShutdown}, want: true},
		{name: "crash shutdown", err: &pgconn.PgError{Code: pgerrcode.CrashShutdown}, want: true},
		{name: "cannot connect", err: &pgconn.PgError{Code: pgerrcode.CannotConnectNow}, want: true},
		{name: "unique violation", err: &pgconn.PgError{Code: pgerrcode.UniqueViolation}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Unavailable(tt.err); got != tt.want {
				t.Fatalf("Unavailable() = %v, want %v", got, tt.want)
			}
		})
	}
}
