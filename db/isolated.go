package db

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrUnreachable means PostgreSQL did not accept a connection.
var ErrUnreachable = errors.New("postgres unreachable")

// OpenIsolated creates a migrated database and returns a pool for it.
// cleanup closes that pool and drops the database.
// An empty adminURL uses the local fixture database.
func OpenIsolated(ctx context.Context, adminURL string) (*pgxpool.Pool, func(), error) {
	if adminURL == "" {
		adminURL = "postgres://e2b:e2b@localhost:5432/e2b"
	}
	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %s", ErrUnreachable, err.Error())
	}
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := admin.Ping(pingCtx); err != nil {
		admin.Close()
		return nil, nil, fmt.Errorf("%w: %s", ErrUnreachable, err.Error())
	}

	name := "sandbox_api_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
		admin.Close()
		return nil, nil, fmt.Errorf("create database: %w", err)
	}
	drop := func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+ident+" WITH (FORCE)")
		admin.Close()
	}

	parsed, err := url.Parse(adminURL)
	if err != nil {
		drop()
		return nil, nil, fmt.Errorf("parse database url: %w", err)
	}
	parsed.Path = "/" + name
	pool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		drop()
		return nil, nil, fmt.Errorf("%w: %s", ErrUnreachable, err.Error())
	}
	cleanup := func() {
		pool.Close()
		drop()
	}
	if err := Up(ctx, pool); err != nil {
		cleanup()
		return nil, nil, err
	}
	return pool, cleanup, nil
}
